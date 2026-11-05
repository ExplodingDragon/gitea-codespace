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
	route      *componentv1.GatewayRuntimeRoute
	access     func(context.Context, agentv1connect.AgentAccessServiceAccessServerStream) error
	capability chan agentv1.AccessCapability
	open       chan *agentv1.OpenAccess
}

func (s *backendTestServer) IssueAgentAccess(_ context.Context, request *componentv1.IssueAgentAccessRequest) (*componentv1.IssueAgentAccessResponse, error) {
	s.capability <- request.Capability
	return &componentv1.IssueAgentAccessResponse{Runtime: s.route, Ticket: "test-ticket"}, nil
}

func (s *backendTestServer) Access(ctx context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
	request, err := stream.Receive()
	if err != nil {
		return err
	}
	s.open <- request.GetOpen()
	if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Accepted{Accepted: true}}); err != nil {
		return err
	}
	return s.access(ctx, stream)
}

func newBackendTestClient(t *testing.T, handle func(context.Context, agentv1connect.AgentAccessServiceAccessServerStream) error) (*AgentBackend, *backendTestServer) {
	t.Helper()
	route := &componentv1.GatewayRuntimeRoute{RuntimeUuid: "runtime", SiteUid: "site", ResourceUid: "resource", PodUid: "pod", AgentAddress: "agent:8444"}
	service := &backendTestServer{
		route: route, access: handle,
		capability: make(chan agentv1.AccessCapability, 1),
		open:       make(chan *agentv1.OpenAccess, 1),
	}
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
	return backend, service
}

func TestAgentCommandCloseReleasesBlockedOutput(t *testing.T) {
	serverDone := make(chan struct{})
	backend, _ := newBackendTestClient(t, func(ctx context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
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

func TestAgentBackendPreservesSSHProcessKind(t *testing.T) {
	tests := []struct {
		name       string
		capability agentv1.AccessCapability
		open       func(context.Context, *AgentBackend) (WorkspaceCommandSession, error)
		assertOpen func(*testing.T, *agentv1.OpenAccess)
	}{
		{
			name:       "shell without terminal",
			capability: agentv1.AccessCapability_ACCESS_CAPABILITY_SHELL,
			open: func(ctx context.Context, backend *AgentBackend) (WorkspaceCommandSession, error) {
				return backend.OpenWorkspaceShell(ctx, WorkspaceShellRequest{RuntimeUUID: "runtime"})
			},
			assertOpen: func(t *testing.T, open *agentv1.OpenAccess) {
				require.NotNil(t, open.GetShell())
				require.Nil(t, open.GetShell().Terminal)
			},
		},
		{
			name:       "command with terminal",
			capability: agentv1.AccessCapability_ACCESS_CAPABILITY_COMMAND,
			open: func(ctx context.Context, backend *AgentBackend) (WorkspaceCommandSession, error) {
				return backend.OpenWorkspaceCommand(ctx, WorkspaceCommandRequest{RuntimeUUID: "runtime", Command: "printf test", Interactive: true, Cols: 91, Rows: 37})
			},
			assertOpen: func(t *testing.T, open *agentv1.OpenAccess) {
				require.Equal(t, "printf test", open.GetCommand().GetCommand())
				require.Equal(t, "xterm-256color", open.GetCommand().GetTerminal().GetTerm())
				require.Equal(t, uint32(91), open.GetCommand().GetTerminal().GetSize().GetColumns())
				require.Equal(t, uint32(37), open.GetCommand().GetTerminal().GetSize().GetRows())
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend, service := newBackendTestClient(t, func(_ context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
				return stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ExitStatus{ExitStatus: 0}})
			})
			session, err := test.open(t.Context(), backend)
			require.NoError(t, err)
			t.Cleanup(func() { _ = session.Close() })
			require.Equal(t, test.capability, <-service.capability)
			test.assertOpen(t, <-service.open)
			require.NoError(t, session.Wait())
		})
	}
}

func TestAgentTCPHalfCloseReceivesResponse(t *testing.T) {
	backend, _ := newBackendTestClient(t, func(_ context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
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
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, conn.(interface{ CloseWrite() error }).CloseWrite())
	response, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Equal(t, "response:hello", string(response))
}
