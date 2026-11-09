// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayRouteMetadataUpdateKeepsConnections(t *testing.T) {
	t.Parallel()

	routes := newRouteStore(gatewayTestBackend{})
	t.Cleanup(routes.Close)
	route := endpointRoute{
		SiteUID: "site-1", GiteaWebURL: "https://old.example.test", PodUID: "pod-1", AgentAddress: "agent-1",
		CodespaceUUID: "codespace-1", EndpointID: "app", Label: "App", Public: true,
	}
	if err := routes.Put(route); err != nil {
		t.Fatal(err)
	}
	_, request, release, ok := routes.BeginProxy(httptest.NewRequest("GET", "http://gateway.example.test", nil), route.CodespaceUUID, route.EndpointID)
	if !ok {
		t.Fatal("begin proxy")
	}
	defer release()
	route.GiteaWebURL = "https://new.example.test"
	if err := routes.Put(route); err != nil {
		t.Fatal(err)
	}
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
	route.PodUID = "pod-2"
	if err := routes.Put(route); err != nil {
		t.Fatal(err)
	}
	select {
	case <-request.Context().Done():
	default:
		t.Fatal("target route update did not cancel the proxy")
	}
}

func TestGatewayRoutePreservesRuntimeIdentity(t *testing.T) {
	routes := newRouteStore(gatewayTestBackend{})
	t.Cleanup(routes.Close)
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{{
		SiteUID: "site-1", ResourceUID: "resource-1", RuntimeUUID: "codespace-1",
		GiteaWebURL: "https://gitea.example.test", PodUID: "pod-1", AgentAddress: "agent:8444", TargetVersion: 7,
		Endpoints: []endpointSnapshot{{EndpointID: "app", Label: "Application", Public: true}},
	}}))

	route, found := routes.Get("codespace-1", "app")
	require.True(t, found)
	require.Equal(t, "resource-1", route.ResourceUID)
	require.Equal(t, int64(7), route.TargetVersion)
	runtime, found := routes.RuntimeTarget("codespace-1")
	require.True(t, found)
	require.Equal(t, "resource-1", runtime.ResourceUID)
}

func TestGatewayRouteSnapshotLogsOnlyChanges(t *testing.T) {
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	routes := newRouteStore(gatewayTestBackend{})
	t.Cleanup(routes.Close)
	snapshot := runtimeSnapshot{
		SiteUID: "site-1", ResourceUID: "resource-1", RuntimeUUID: "codespace-1",
		GiteaWebURL: "https://gitea.example.test", PodUID: "pod-1", AgentAddress: "agent:8444", TargetVersion: 1,
		Endpoints: []endpointSnapshot{{EndpointID: "app", Label: "Application", Public: true}},
	}
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))
	snapshot.TargetVersion++
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))

	require.Equal(t, 2, strings.Count(output.String(), `"event":"gateway_routes_updated"`))
}
