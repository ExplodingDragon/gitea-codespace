// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
	accept     bool
}

func TestAgentHTTPReusesConnectionAndRedialsAfterTargetClose(t *testing.T) {
	idle := make(chan net.Conn, 4)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	upstream.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateIdle {
			idle <- conn
		}
	}
	upstream.Start()
	defer upstream.Close()
	var opened atomic.Int32
	targetEOF := make(chan struct{}, 4)
	backend, _ := newBackendTestClient(t, func(ctx context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
		opened.Add(1)
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp4", upstream.Listener.Addr().String())
		if err != nil {
			return err
		}
		defer connection.Close()
		stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
		defer stop()
		go func() {
			for {
				frame, err := stream.Receive()
				if err != nil {
					_ = connection.Close()
					return
				}
				if frame.GetStdinEof() {
					_ = connection.(*net.TCPConn).CloseWrite()
					return
				}
				if _, err := connection.Write(frame.GetStdin()); err != nil {
					_ = connection.Close()
					return
				}
			}
		}()
		buffer := make([]byte, 32*1024)
		for {
			n, err := connection.Read(buffer)
			if n > 0 {
				if sendErr := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Stdout{Stdout: buffer[:n]}}); sendErr != nil {
					return sendErr
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_StdoutEof{StdoutEof: true}}); err != nil {
						return err
					}
					targetEOF <- struct{}{}
					return stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ExitStatus{ExitStatus: 0}})
				}
				return err
			}
		}
	})
	routes := newRouteStore(backend)
	defer routes.Close()
	route := endpointRoute{SiteUID: "site", CodespaceUUID: "runtime", GiteaWebURL: "https://gitea.test", EndpointID: "app", Public: true}
	require.NoError(t, routes.Put(route))
	transport, err := routes.Transport(route)
	require.NoError(t, err)
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	for i := range 3 {
		response, err := client.Get("http://workspace.test/")
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, "ok", string(body))
		var conn net.Conn
		select {
		case conn = <-idle:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream did not become idle")
		}
		if i == 1 {
			require.Equal(t, int32(1), opened.Load(), "HTTP connection must be reused")
			require.NoError(t, conn.Close())
			select {
			case <-targetEOF:
			case <-time.After(3 * time.Second):
				t.Fatal("target EOF was hidden behind open input")
			}
		}
	}
	require.Equal(t, int32(2), opened.Load())
}

func TestAgentStreamDiagnosticsAndExitStatus(t *testing.T) {
	for _, status := range []int32{0, 7} {
		t.Run(fmt.Sprintf("exit %d", status), func(t *testing.T) {
			backend, _ := newBackendTestClient(t, func(_ context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
				for _, frame := range []*agentv1.AccessResponse{
					{Frame: &agentv1.AccessResponse_Stderr{Stderr: []byte("diagnostic")}},
					{Frame: &agentv1.AccessResponse_Stdout{Stdout: []byte("payload")}},
					{Frame: &agentv1.AccessResponse_ExitStatus{ExitStatus: status}},
				} {
					if err := stream.Send(frame); err != nil {
						return err
					}
				}
				return nil
			})
			conn, err := backend.OpenWorkspaceSFTP(t.Context(), workspaceSFTPRequest{RuntimeUUID: "runtime"})
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			body, err := io.ReadAll(conn)
			require.Equal(t, "payload", string(body))
			if status == 0 {
				require.NoError(t, err)
			} else {
				var exit *workspaceCommandExitError
				require.ErrorAs(t, err, &exit)
				require.Equal(t, int(status), exit.Status)
				require.ErrorContains(t, err, "diagnostic")
			}
		})
	}
}

func TestAgentClientCancellationClosesActiveStreams(t *testing.T) {
	backend, _ := newBackendTestClient(t, func(ctx context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
		if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Stdout{Stdout: []byte("ready")}}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	first, err := backend.OpenWorkspaceTCP(t.Context(), "runtime", 8000)
	require.NoError(t, err)
	second, err := backend.OpenWorkspaceTCP(t.Context(), "runtime", 8001)
	require.NoError(t, err)
	defer func() { _ = second.Close() }()
	require.NoError(t, first.Close())
	require.NoError(t, second.SetReadDeadline(time.Now().Add(3*time.Second)))
	buffer := make([]byte, 5)
	_, err = io.ReadFull(second, buffer)
	require.NoError(t, err)
	require.Equal(t, "ready", string(buffer))
	backend.ClosePod("pod")
	_, err = second.Read(buffer)
	require.ErrorIs(t, err, context.Canceled)
}

func TestAgentOpenReportsTargetFailure(t *testing.T) {
	backend, service := newBackendTestClient(t, func(context.Context, agentv1connect.AgentAccessServiceAccessServerStream) error {
		return connect.NewError(connect.CodeUnavailable, "connection refused")
	})
	service.accept = false
	_, err := backend.OpenWorkspaceTCP(t.Context(), "runtime", 8000)
	require.ErrorContains(t, err, "connection refused")
}

func TestAgentReverseListenerClaimsIndependentConnection(t *testing.T) {
	var calls atomic.Int32
	backend, _ := newBackendTestClient(t, func(ctx context.Context, stream agentv1connect.AgentAccessServiceAccessServerStream) error {
		if calls.Add(1) == 1 {
			if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ReverseTcpConnection{ReverseTcpConnection: &agentv1.ReverseTCPConnection{
				ConnectionId: "connection", OriginHost: "127.0.0.1", OriginPort: 42000,
			}}}); err != nil {
				return err
			}
			if _, err := stream.Receive(); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			return stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ExitStatus{ExitStatus: 0}})
		}
		for _, frame := range []*agentv1.AccessResponse{
			{Frame: &agentv1.AccessResponse_Stdout{Stdout: []byte("usbip")}},
			{Frame: &agentv1.AccessResponse_StdoutEof{StdoutEof: true}},
			{Frame: &agentv1.AccessResponse_ExitStatus{ExitStatus: 0}},
		} {
			if err := stream.Send(frame); err != nil {
				return err
			}
		}
		return nil
	})
	listener, err := backend.OpenWorkspaceReverseTCP(t.Context(), "runtime", "usbip.workstation", 3240)
	require.NoError(t, err)
	defer listener.Close()
	accepted, err := listener.Accept(t.Context())
	require.NoError(t, err)
	defer accepted.Connection.Close()
	require.Equal(t, "127.0.0.1", accepted.OriginHost)
	require.EqualValues(t, 42000, accepted.OriginPort)
	output, err := io.ReadAll(accepted.Connection)
	require.NoError(t, err)
	require.Equal(t, "usbip", string(output))
	require.NoError(t, listener.Close())
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
	if s.accept {
		response := &agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Accepted{Accepted: true}}
		if request.GetOpen().GetReverseTcpListen() != nil {
			response.Frame = &agentv1.AccessResponse_ReverseTcpReady{ReverseTcpReady: &agentv1.ReverseTCPReady{Port: 43001}}
		}
		if err := stream.Send(response); err != nil {
			return err
		}
	}
	return s.access(ctx, stream)
}

func newBackendTestClient(t *testing.T, handle func(context.Context, agentv1connect.AgentAccessServiceAccessServerStream) error) (*agentBackend, *backendTestServer) {
	t.Helper()
	route := &componentv1.GatewayRuntimeRoute{RuntimeUuid: "runtime", SiteUid: "site", ResourceUid: "resource", PodUid: "pod", AgentAddress: "agent:8444"}
	service := &backendTestServer{
		route: route, access: handle, accept: true,
		capability: make(chan agentv1.AccessCapability, 16),
		open:       make(chan *agentv1.OpenAccess, 16),
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
	ctx, cancel := context.WithCancel(t.Context())
	backend := &agentBackend{Manager: componentv1connect.NewComponentServiceClient(client), clients: map[string]*agentClient{
		"pod": {ctx: ctx, cancel: cancel, address: route.AgentAddress, siteUID: route.SiteUid, resourceUID: route.ResourceUid,
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
	session, err := backend.OpenWorkspaceCommand(ctx, workspaceCommandRequest{RuntimeUUID: "runtime", Command: "test"})
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
		open       func(context.Context, *agentBackend) (workspaceCommandSession, error)
		assertOpen func(*testing.T, *agentv1.OpenAccess)
	}{
		{
			name:       "shell without terminal",
			capability: agentv1.AccessCapability_ACCESS_CAPABILITY_SHELL,
			open: func(ctx context.Context, backend *agentBackend) (workspaceCommandSession, error) {
				return backend.OpenWorkspaceShell(ctx, workspaceShellRequest{RuntimeUUID: "runtime"})
			},
			assertOpen: func(t *testing.T, open *agentv1.OpenAccess) {
				require.NotNil(t, open.GetShell())
				require.Nil(t, open.GetShell().Terminal)
			},
		},
		{
			name:       "command with terminal",
			capability: agentv1.AccessCapability_ACCESS_CAPABILITY_COMMAND,
			open: func(ctx context.Context, backend *agentBackend) (workspaceCommandSession, error) {
				return backend.OpenWorkspaceCommand(ctx, workspaceCommandRequest{RuntimeUUID: "runtime", Command: "printf test", Interactive: true, Cols: 91, Rows: 37})
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
		if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_StdoutEof{StdoutEof: true}}); err != nil {
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
