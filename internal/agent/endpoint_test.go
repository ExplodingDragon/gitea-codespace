// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"github.com/stretchr/testify/require"
)

func TestRuntimeEndpointStateUpdatesPublishedTarget(t *testing.T) {
	directory := t.TempDir()
	identity := VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"}
	journal, err := OpenJournal(directory, identity)
	require.NoError(t, err)
	runtime := &Runtime{Journal: journal}
	require.NoError(t, runtime.replaceConfiguredEndpoints([]*codespacev1.RuntimeEndpoint{{EndpointId: "port-3000", Label: "API", Port: 3000}}))
	target := accessTargetForContainer(1, "container", "/workspaces/project", true, runtime.endpoints, nil)
	runtime.setAccessState(target, nil)

	endpoints, err := runtime.setEndpoint(&codespacev1.RuntimeEndpoint{EndpointId: "port-8080", Label: "Preview", Public: true, Port: 8080})
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
	updated := runtime.CurrentAccessTarget()
	require.EqualValues(t, 2, updated.Version)
	require.Equal(t, "/workspaces/project", updated.WorkspaceFolder)
	require.Len(t, updated.Endpoints, 3)
	require.True(t, updated.Endpoints[2].Endpoint.Public)
	require.Equal(t, runtimeendpoint.WorkspaceEndpointID, updated.Endpoints[0].Endpoint.EndpointId)
	require.Empty(t, accessTargetForContainer(1, "container", "/workspaces/project", false, nil, nil).Endpoints)

	runtime.setAccessState(accessTargetForContainer(3, "container", "/workspaces/project", false, runtime.endpoints, nil), nil)
	_, err = runtime.setEndpoint(&codespacev1.RuntimeEndpoint{EndpointId: "port-9090", Label: "Metrics", Port: 9090})
	require.NoError(t, err)
	updated = runtime.CurrentAccessTarget()
	require.Len(t, updated.Endpoints, 3)
	endpointIDs := make([]string, len(updated.Endpoints))
	for index, endpoint := range updated.Endpoints {
		endpointIDs[index] = endpoint.Endpoint.EndpointId
	}
	require.Equal(t, []string{
		runtimeendpoint.PortEndpointID(3000),
		runtimeendpoint.PortEndpointID(8080),
		runtimeendpoint.PortEndpointID(9090),
	}, endpointIDs)

	require.NoError(t, journal.Close())
	journal, err = OpenJournal(directory, identity)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	recovered := &Runtime{Journal: journal}
	endpoints, err = recovered.listEndpoints()
	require.NoError(t, err)
	require.Len(t, endpoints, 3)
	require.EqualValues(t, 9090, endpoints[2].Port)
}

func TestRuntimeEndpointServiceOverUnixSocket(t *testing.T) {
	journal, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	rpcServer := connect.NewServer()
	agentv1connect.RegisterRuntimeEndpointServiceHandler(rpcServer, &RuntimeEndpointServer{Runtime: &Runtime{Journal: journal}})
	handler := http.NewServeMux()
	connecthttp.Mount(handler, rpcServer)
	path := "/" + agentv1connect.RuntimeEndpointServiceName + "/"

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
	client := agentv1connect.NewRuntimeEndpointServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: transport}, "http://runtime-agent")))
	t.Cleanup(transport.CloseIdleConnections)

	_, err = client.Set(t.Context(), &agentv1.RuntimeEndpointServiceSetRequest{ProtocolVersion: 1, Endpoint: &codespacev1.RuntimeEndpoint{EndpointId: "port-8080", Label: "Preview", Port: 8080}})
	require.NoError(t, err)
	listed, err := client.List(t.Context(), &agentv1.RuntimeEndpointServiceListRequest{ProtocolVersion: 1})
	require.NoError(t, err)
	require.Len(t, listed.Endpoints, 1)
	_, err = client.Delete(t.Context(), &agentv1.RuntimeEndpointServiceDeleteRequest{ProtocolVersion: 1, EndpointId: "port-8080"})
	require.NoError(t, err)
	listed, err = client.List(t.Context(), &agentv1.RuntimeEndpointServiceListRequest{ProtocolVersion: 1})
	require.NoError(t, err)
	require.Empty(t, listed.Endpoints)
}
