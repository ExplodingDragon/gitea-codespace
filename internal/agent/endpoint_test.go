// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/devcontainerruntime"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeEndpointStateUpdatesPublishedTarget(t *testing.T) {
	directory := t.TempDir()
	identity := VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"}
	journal, err := OpenJournal(directory, identity)
	require.NoError(t, err)
	runtime := &Runtime{Journal: journal}
	require.NoError(t, runtime.replaceConfiguredEndpoints([]*codespacev1.RuntimeEndpoint{{EndpointId: "port-3000", Label: "API", Port: 3000}}))
	state := &devcontainer.State{
		Version: devcontainer.StateFormatVersion, ID: "environment", OwnerID: "runtime",
		PrimaryContainerID: "container", ConfigurationPath: "/workspaces/project/.devcontainer.json", ConfigurationSHA256: strings.Repeat("a", 64),
		Workspace: "/workspaces/project", WorkspaceFolder: "/workspaces/project", RemoteWorkdir: "/workspaces/project", RemoteUser: "developer",
	}
	var stages []codespacev1.RuntimeBootStage
	require.NoError(t, runtime.publishAccess(1, &devcontainerruntime.Outcome{Environment: state, WebIDEStarted: true}, nil,
		func(stage codespacev1.RuntimeBootStage, target *agentv1.AccessTarget) {
			stages = append(stages, stage)
			if stage == codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PUBLISH_READY {
				require.Nil(t, runtime.CurrentAccessTarget())
			} else {
				require.True(t, proto.Equal(target, runtime.CurrentAccessTarget()))
			}
		}, io.Discard))
	require.Equal(t, []codespacev1.RuntimeBootStage{
		codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PUBLISH_READY, codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY,
	}, stages)
	target := runtime.CurrentAccessTarget()
	accessCtx, release, err := runtime.networkAccessContext(t.Context(), target)
	require.NoError(t, err)
	defer release()
	t.Cleanup(runtime.closeNetworkAccess)

	endpoints, err := runtime.setEndpoint(&codespacev1.RuntimeEndpoint{EndpointId: "port-8080", Label: "Preview", Public: true, Port: 8080})
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
	updated := runtime.CurrentAccessTarget()
	require.EqualValues(t, 2, updated.Version)
	require.Equal(t, "/workspaces/project", updated.WorkspaceFolder)
	require.Len(t, updated.Endpoints, 3)
	require.True(t, updated.Endpoints[2].Endpoint.Public)
	require.Equal(t, runtimeendpoint.WorkspaceEndpointID, updated.Endpoints[0].Endpoint.EndpointId)
	require.NoError(t, accessCtx.Err(), "endpoint publication preserves existing network streams")
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
