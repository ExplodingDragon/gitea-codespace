// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testGatewayRuntime() runtimeSnapshot {
	return runtimeSnapshot{
		SiteUID: "site-1", ResourceUID: "11111111-1111-4111-8111-111111111111", RuntimeKey: "site-1/42",
		GiteaWebURL: "https://gitea.example.test", PodUID: "pod-1", AgentAddress: "agent:8444", TargetVersion: 1,
		Endpoints: []endpointSnapshot{{EndpointID: "workspace", Label: "Workspace"}},
	}
}

func TestGatewayRouteMetadataUpdateKeepsConnections(t *testing.T) {
	t.Parallel()

	routes := newRouteStore(gatewayTestBackend{})
	t.Cleanup(routes.Close)
	snapshot := testGatewayRuntime()
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))
	route, ok := routes.Get(snapshot.RuntimeKey, "workspace")
	require.True(t, ok)
	_, request, release, ok := routes.BeginProxy(httptest.NewRequest("GET", "http://gateway.example.test", nil), route.RuntimeKey, route.EndpointID)
	if !ok {
		t.Fatal("begin proxy")
	}
	defer release()
	snapshot.GiteaWebURL = "https://new.example.test"
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))
	select {
	case <-request.Context().Done():
		t.Fatal("metadata-only route update canceled the proxy")
	default:
	}

	transport, err := routes.Transport(route)
	if err != nil {
		t.Fatal(err)
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("response header timeout = %s", transport.ResponseHeaderTimeout)
	}
	snapshot.PodUID = "pod-2"
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))
	select {
	case <-request.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("target route update did not cancel the proxy")
	}
}

func TestGatewayRoutePreservesRuntimeIdentity(t *testing.T) {
	routes := newRouteStore(gatewayTestBackend{})
	t.Cleanup(routes.Close)
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{{
		SiteUID: "site-1", ResourceUID: "resource-1", RuntimeKey: "site-1/1",
		GiteaWebURL: "https://gitea.example.test", PodUID: "pod-1", AgentAddress: "agent:8444", TargetVersion: 7,
		Endpoints: []endpointSnapshot{{EndpointID: "app", Label: "Application", Public: true}},
	}}))

	route, found := routes.Get("site-1/1", "app")
	require.True(t, found)
	require.Equal(t, "resource-1", route.ResourceUID)
	require.Equal(t, int64(7), route.TargetVersion)
	runtime, found := routes.RuntimeTarget("site-1/1")
	require.True(t, found)
	require.Equal(t, "resource-1", runtime.ResourceUID)
	other := runtime
	other.SiteUID, other.ResourceUID, other.RuntimeKey = "site-2", "resource-2", "site-2/1"
	other.GiteaWebURL = "https://other.example.test"
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{runtime, other}))
	for _, snapshot := range []runtimeSnapshot{runtime, other} {
		key, ok := routes.ResolveAlias(snapshot.ResourceUID)
		require.True(t, ok)
		require.Equal(t, snapshot.RuntimeKey, key)
		target, ok := routes.Get(key, "app")
		require.True(t, ok)
		require.Equal(t, snapshot.SiteUID, target.SiteUID)
	}
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{other}))
	_, found = routes.ResolveAlias(runtime.ResourceUID)
	require.False(t, found)
}

func TestGatewayRouteSnapshotLogsOnlyChanges(t *testing.T) {
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	routes := newRouteStore(gatewayTestBackend{})
	t.Cleanup(routes.Close)
	snapshot := runtimeSnapshot{
		SiteUID: "site-1", ResourceUID: "resource-1", RuntimeKey: "site-1/1",
		GiteaWebURL: "https://gitea.example.test", PodUID: "pod-1", AgentAddress: "agent:8444", TargetVersion: 1,
		Endpoints: []endpointSnapshot{{EndpointID: "app", Label: "Application", Public: true}},
	}
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))
	snapshot.TargetVersion++
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))

	require.Equal(t, 2, strings.Count(output.String(), `"event":"gateway_routes_updated"`))
}
