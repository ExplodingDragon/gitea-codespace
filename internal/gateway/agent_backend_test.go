// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"gitea.dev/codespace/internal/rpc/component/v1/componentv1connect"
	"github.com/stretchr/testify/require"
)

type backendTestServer struct {
	componentv1connect.UnimplementedComponentServiceHandler
	route  *componentv1.GatewayRuntimeRoute
	access func(context.Context, agentv1connect.AgentAccessServiceAccessServerStream) error
}

func (s *backendTestServer) IssueAgentAccess(context.Context, *componentv1.IssueAgentAccessRequest) (*componentv1.IssueAgentAccessResponse, error) {
	return &componentv1.IssueAgentAccessResponse{Runtime: s.route, Ticket: "test-ticket"}, nil
}

func (s *backendTestServer) Access(ctx context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
	if _, err := stream.Receive(); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Accepted{Accepted: true}}); err != nil {
		return err
	}
	return s.access(ctx, stream)
}

func newBackendTestClient(t *testing.T, handle func(context.Context, agentv1connect.AgentAccessServiceAccessServerStream) error) *AgentBackend {
	t.Helper()
	route := &componentv1.GatewayRuntimeRoute{RuntimeUuid: "runtime", SiteUid: "site", ResourceUid: "resource", PodUid: "pod", AgentAddress: "agent:8444"}
	service := &backendTestServer{route: route, access: handle}
	rpc := connect.NewServer()
	componentv1connect.RegisterComponentServiceHandler(rpc, service)
	agentv1connect.RegisterAgentAccessServiceHandler(rpc, service)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, rpc)
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client := connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL, connecthttp.WithGRPC()))
	backend := &AgentBackend{Manager: componentv1connect.NewComponentServiceClient(client), clients: map[string]*agentClient{
		"pod": {address: route.AgentAddress, siteUID: route.SiteUid, resourceUID: route.ResourceUid,
			transport: server.Client().Transport.(*http.Transport), access: agentv1connect.NewAgentAccessServiceClient(client)},
	}}
	t.Cleanup(backend.Close)
	return backend
}

func TestAgentCommandCloseReleasesBlockedOutput(t *testing.T) {
	serverDone := make(chan struct{})
	backend := newBackendTestClient(t, func(ctx context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
		defer close(serverDone)
		if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Stdout{Stdout: []byte("unread output")}}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	session, err := backend.OpenWorkspaceCommand(ctx, WorkspaceCommandRequest{RuntimeUUID: "runtime", Command: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	_, err = io.ReadFull(session.Stdout(), make([]byte, 1))
	require.NoError(t, err)
	firstClose := session.Close()
	require.Equal(t, firstClose, session.Close())
	waited := make(chan error, 1)
	go func() { waited <- session.Wait() }()
	select {
	case err := <-waited:
		require.Error(t, err)
	case <-ctx.Done():
		t.Fatal("command output receiver remained blocked after close")
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("access RPC remained active after close")
	}
}

func TestAgentTCPHalfCloseReceivesResponse(t *testing.T) {
	backend := newBackendTestClient(t, func(_ context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
		var request []byte
		for {
			frame, err := stream.Receive()
			if err != nil {
				return err
			}
			request = append(request, frame.GetStdin()...)
			if frame.GetStdinEof() {
				break
			}
		}
		if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Stdout{Stdout: append([]byte("response:"), request...)}}); err != nil {
			return err
		}
		return stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ExitStatus{ExitStatus: 0}})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := backend.OpenWorkspaceTCP(ctx, "runtime", 8000)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, conn.(interface{ CloseWrite() error }).CloseWrite())
	response, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Equal(t, "response:hello", string(response))
}
