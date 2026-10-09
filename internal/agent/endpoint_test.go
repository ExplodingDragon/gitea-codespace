// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"github.com/stretchr/testify/require"
)

func TestRuntimeEndpointStateUpdatesPublishedTarget(t *testing.T) {
	directory := t.TempDir()
	identity := VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"}
	journal, err := OpenJournal(directory, identity)
	require.NoError(t, err)
	runtime := &Runtime{Journal: journal}
	require.NoError(t, runtime.replaceConfiguredEndpoints([]*codespacev1.RuntimeEndpoint{{EndpointId: "port-3000", Label: "API", Port: 3000}}))
	target := accessTargetForContainer(1, "container", runtime.endpoints)
	runtime.setAccessState(target, nil)

	endpoints, err := runtime.setEndpoint(&codespacev1.RuntimeEndpoint{EndpointId: "port-8080", Label: "Preview", Public: true, Port: 8080})
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
	updated := runtime.CurrentAccessTarget()
	require.EqualValues(t, 2, updated.Version)
	require.Len(t, updated.Endpoints, 3)
	require.True(t, updated.Endpoints[2].Endpoint.Public)

	require.NoError(t, journal.Close())
	journal, err = OpenJournal(directory, identity)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	recovered := &Runtime{Journal: journal}
	endpoints, err = recovered.listEndpoints()
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
	require.EqualValues(t, 8080, endpoints[1].Port)
}

func TestRuntimeEndpointServiceOverUnixSocket(t *testing.T) {
	journal, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })

	path, handler := agentv1connect.NewRuntimeEndpointServiceHandler(&RuntimeEndpointServer{Runtime: &Runtime{Journal: journal}})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := &http.Server{Handler: mux}
	socket := filepath.Join(t.TempDir(), "endpoint.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		require.NoError(t, server.Shutdown(context.Background()))
		require.ErrorIs(t, <-done, http.ErrServerClosed)
	})

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := agentv1connect.NewRuntimeEndpointServiceClient(&http.Client{Transport: transport}, "http://runtime-agent")
	t.Cleanup(transport.CloseIdleConnections)

	_, err = client.Set(t.Context(), connect.NewRequest(&agentv1.RuntimeEndpointServiceSetRequest{ProtocolVersion: 1, Endpoint: &codespacev1.RuntimeEndpoint{EndpointId: "port-8080", Label: "Preview", Port: 8080}}))
	require.NoError(t, err)
	listed, err := client.List(t.Context(), connect.NewRequest(&agentv1.RuntimeEndpointServiceListRequest{ProtocolVersion: 1}))
	require.NoError(t, err)
	require.Len(t, listed.Msg.Endpoints, 1)
	_, err = client.Delete(t.Context(), connect.NewRequest(&agentv1.RuntimeEndpointServiceDeleteRequest{ProtocolVersion: 1, EndpointId: "port-8080"}))
	require.NoError(t, err)
	listed, err = client.List(t.Context(), connect.NewRequest(&agentv1.RuntimeEndpointServiceListRequest{ProtocolVersion: 1}))
	require.NoError(t, err)
	require.Empty(t, listed.Msg.Endpoints)
}
