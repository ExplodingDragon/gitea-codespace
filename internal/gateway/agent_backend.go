// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	identitypkg "gitea.dev/codespace/internal/identity"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"gitea.dev/codespace/internal/rpc/component/v1/componentv1connect"
	"gitea.dev/codespace/internal/transport"
)

const agentAccessSetupTimeout = 15 * time.Second

// The Agent backend asks Manager for a fresh target-bound ticket before
// adapting each Gateway data stream to the current Runtime Agent.
type agentBackend struct {
	Manager      componentv1connect.ComponentServiceClient
	Certificates *transport.FileCertificates

	mu      sync.Mutex
	clients map[string]*agentClient
}

type agentClient struct {
	ctx         context.Context
	cancel      context.CancelFunc
	address     string
	siteUID     string
	resourceUID string
	transport   *http.Transport
	access      agentv1connect.AgentAccessServiceClient
}

func (c *agentClient) close() {
	c.cancel()
	c.transport.CloseIdleConnections()
}

type agentAccessStream struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stream   agentv1connect.AgentAccessServiceAccessClientStream
	mu       sync.Mutex
	once     sync.Once
	closeErr error
}

func (b *agentBackend) open(ctx context.Context, runtimeUUID string, capability agentv1.AccessCapability, endpointID string, open *agentv1.OpenAccess) (_ *agentAccessStream, resultErr error) {
	if b == nil || b.Manager == nil {
		return nil, fmt.Errorf("agent backend is unavailable")
	}
	streamCtx, cancelCause := context.WithCancelCause(ctx)
	cancel := func() { cancelCause(context.Canceled) }
	setupTimer := time.AfterFunc(agentAccessSetupTimeout, func() { cancelCause(context.DeadlineExceeded) })
	established := false
	defer func() {
		if !established {
			setupTimer.Stop()
			resultErr = errors.Join(resultErr, context.Cause(streamCtx))
			cancel()
		}
	}()
	response, err := b.Manager.IssueAgentAccess(streamCtx, &componentv1.IssueAgentAccessRequest{ProtocolVersion: 1, RuntimeUuid: runtimeUUID, Capability: capability, EndpointId: endpointID})
	if err != nil {
		return nil, wrapGatewayUpstreamError("ticket_issue", err)
	}
	route := response.Runtime
	if route == nil || route.RuntimeUuid != runtimeUUID || route.SiteUid == "" || route.ResourceUid == "" || route.PodUid == "" || route.AgentAddress == "" || response.Ticket == "" {
		return nil, wrapGatewayUpstreamError("ticket_issue", fmt.Errorf("manager returned an incomplete Agent target"))
	}
	target := runtimeSnapshot{
		SiteUID: route.SiteUid, ResourceUID: route.ResourceUid, RuntimeUUID: route.RuntimeUuid,
		PodUID: route.PodUid, AgentAddress: route.AgentAddress, TargetVersion: route.TargetVersion,
	}
	client, err := b.clientFor(route)
	if err != nil {
		return nil, wrapGatewayUpstreamTargetError("agent_connect", target, err)
	}
	stopClient := context.AfterFunc(client.ctx, cancel)
	streamCancel := cancel
	cancel = func() {
		stopClient()
		streamCancel()
	}
	stream, err := client.access.Access(streamCtx)
	if err != nil {
		return nil, wrapGatewayUpstreamTargetError("agent_connect", target, err)
	}
	open.Ticket = response.Ticket
	if err := stream.Send(&agentv1.AccessRequest{ProtocolVersion: 1, Frame: &agentv1.AccessRequest_Open{Open: open}}); err != nil {
		return nil, wrapGatewayUpstreamTargetError("agent_open", target, err)
	}
	accepted, err := stream.Receive()
	if err != nil || !accepted.GetAccepted() {
		if err == nil {
			err = fmt.Errorf("agent rejected the access stream")
		}
		return nil, wrapGatewayUpstreamTargetError("agent_open", target, err)
	}
	if !setupTimer.Stop() {
		return nil, wrapGatewayUpstreamTargetError("agent_open", target, context.DeadlineExceeded)
	}
	established = true
	gatewayLog(streamCtx, slog.LevelDebug, "gateway_agent_access_opened",
		append(gatewayRuntimeLogAttrs(target), slog.String("capability", capability.String()), slog.String("endpoint_id", endpointID))...)
	return &agentAccessStream{ctx: streamCtx, cancel: cancel, stream: stream}, nil
}

func (b *agentBackend) clientFor(route *componentv1.GatewayRuntimeRoute) (*agentClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.clients == nil {
		b.clients = make(map[string]*agentClient)
	}
	if client := b.clients[route.PodUid]; client != nil {
		if client.address == route.AgentAddress && client.siteUID == route.SiteUid && client.resourceUID == route.ResourceUid {
			return client, nil
		}
		client.close()
		delete(b.clients, route.PodUid)
	}
	if b.Certificates == nil {
		return nil, fmt.Errorf("gateway certificate source is unavailable")
	}
	expected := identitypkg.AgentURI(route.SiteUid, route.ResourceUid, route.PodUid)
	tlsConfig, err := b.Certificates.Client(func(identity *url.URL) error {
		if identity.String() != expected {
			return fmt.Errorf("agent identity does not match the current Runtime Pod")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	httpTransport.Proxy = nil
	httpTransport.TLSClientConfig = tlsConfig
	httpClient := &http.Client{Transport: httpTransport}
	ctx, cancel := context.WithCancel(context.Background())
	client := &agentClient{
		ctx: ctx, cancel: cancel,
		address: route.AgentAddress, siteUID: route.SiteUid, resourceUID: route.ResourceUid,
		transport: httpTransport,
		access:    agentv1connect.NewAgentAccessServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, "https://"+route.AgentAddress, connecthttp.WithGRPC()))),
	}
	b.clients[route.PodUid] = client
	return client, nil
}

func (b *agentBackend) ClosePod(podUID string) {
	b.mu.Lock()
	client := b.clients[podUID]
	delete(b.clients, podUID)
	b.mu.Unlock()
	if client != nil {
		client.close()
	}
}

func (b *agentBackend) Close() {
	b.mu.Lock()
	clients := b.clients
	b.clients = make(map[string]*agentClient)
	b.mu.Unlock()
	for _, client := range clients {
		client.close()
	}
}

func (s *agentAccessStream) send(frame *agentv1.AccessRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(frame)
}

func (s *agentAccessStream) close() error {
	s.once.Do(func() {
		s.cancel()
		s.mu.Lock()
		requestErr := s.stream.CloseSend()
		s.mu.Unlock()
		responseErr := s.stream.Close()
		if errors.Is(requestErr, io.EOF) || errors.Is(requestErr, context.Canceled) || errors.Is(requestErr, net.ErrClosed) {
			requestErr = nil
		}
		if errors.Is(responseErr, io.EOF) || errors.Is(responseErr, context.Canceled) || errors.Is(responseErr, net.ErrClosed) {
			responseErr = nil
		}
		s.closeErr = errors.Join(requestErr, responseErr)
	})
	return s.closeErr
}

type agentCommandSession struct {
	access   *agentAccessStream
	stdin    *agentInput
	stdout   *io.PipeReader
	stderr   *io.PipeReader
	done     chan error
	once     sync.Once
	closeErr error
}

type agentInput struct {
	access *agentAccessStream
	once   sync.Once
}

func (w *agentInput) Write(value []byte) (int, error) {
	if err := w.access.send(&agentv1.AccessRequest{ProtocolVersion: 1, Frame: &agentv1.AccessRequest_Stdin{Stdin: append([]byte(nil), value...)}}); err != nil {
		return 0, err
	}
	return len(value), nil
}

func (w *agentInput) Close() error {
	var err error
	w.once.Do(func() {
		err = w.access.send(&agentv1.AccessRequest{ProtocolVersion: 1, Frame: &agentv1.AccessRequest_StdinEof{StdinEof: true}})
	})
	return err
}

func (b *agentBackend) OpenWorkspaceShell(ctx context.Context, request workspaceShellRequest) (workspaceCommandSession, error) {
	shell := &agentv1.Shell{}
	if request.Interactive {
		shell.Terminal = &agentv1.Terminal{Term: "xterm-256color", Size: &agentv1.TerminalSize{Columns: uint32(max(request.Cols, 1)), Rows: uint32(max(request.Rows, 1))}}
	}
	return b.openWorkspaceProcess(ctx, request.RuntimeUUID, agentv1.AccessCapability_ACCESS_CAPABILITY_SHELL, &agentv1.OpenAccess{
		Capability: &agentv1.OpenAccess_Shell{Shell: shell},
	})
}

func (b *agentBackend) OpenWorkspaceCommand(ctx context.Context, request workspaceCommandRequest) (workspaceCommandSession, error) {
	if request.Command == "" {
		return nil, fmt.Errorf("workspace command is required")
	}
	command := &agentv1.Command{Command: request.Command}
	if request.Interactive {
		command.Terminal = &agentv1.Terminal{Term: "xterm-256color", Size: &agentv1.TerminalSize{Columns: uint32(max(request.Cols, 1)), Rows: uint32(max(request.Rows, 1))}}
	}
	return b.openWorkspaceProcess(ctx, request.RuntimeUUID, agentv1.AccessCapability_ACCESS_CAPABILITY_COMMAND, &agentv1.OpenAccess{
		Capability: &agentv1.OpenAccess_Command{Command: command},
	})
}

func (b *agentBackend) openWorkspaceProcess(ctx context.Context, runtimeUUID string, capability agentv1.AccessCapability, open *agentv1.OpenAccess) (workspaceCommandSession, error) {
	access, err := b.open(ctx, runtimeUUID, capability, "", open)
	if err != nil {
		return nil, err
	}
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	session := &agentCommandSession{access: access, stdout: stdoutReader, stderr: stderrReader, done: make(chan error, 1)}
	session.stdin = &agentInput{access: access}
	stopClose := context.AfterFunc(access.ctx, func() {
		_ = stdoutWriter.CloseWithError(access.ctx.Err())
		_ = stderrWriter.CloseWithError(access.ctx.Err())
	})
	go func() {
		defer func() { _ = access.close() }()
		defer func() { _ = stdoutWriter.Close() }()
		defer func() { _ = stderrWriter.Close() }()
		defer stopClose()
		for {
			response, err := access.stream.Receive()
			if err != nil {
				session.done <- err
				return
			}
			switch frame := response.Frame.(type) {
			case *agentv1.AccessResponse_Stdout:
				_, err = stdoutWriter.Write(frame.Stdout)
			case *agentv1.AccessResponse_Stderr:
				_, err = stderrWriter.Write(frame.Stderr)
			case *agentv1.AccessResponse_ExitStatus:
				if frame.ExitStatus == 0 {
					session.done <- nil
				} else {
					session.done <- &workspaceCommandExitError{Status: int(frame.ExitStatus)}
				}
				return
			}
			if err != nil {
				session.done <- err
				return
			}
		}
	}()
	return session, nil
}

func (s *agentCommandSession) Stdin() io.WriteCloser { return s.stdin }
func (s *agentCommandSession) Stdout() io.Reader     { return s.stdout }
func (s *agentCommandSession) Stderr() io.Reader     { return s.stderr }
func (s *agentCommandSession) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("terminal size is invalid")
	}
	return s.access.send(&agentv1.AccessRequest{ProtocolVersion: 1, Frame: &agentv1.AccessRequest_Resize{Resize: &agentv1.TerminalSize{Columns: uint32(cols), Rows: uint32(rows)}}})
}
func (s *agentCommandSession) Signal(signal int) error {
	return s.access.send(&agentv1.AccessRequest{ProtocolVersion: 1, Frame: &agentv1.AccessRequest_Signal{Signal: int32(signal)}})
}
func (s *agentCommandSession) Wait() error { return <-s.done }
func (s *agentCommandSession) Close() error {
	s.once.Do(func() {
		// Canceling the RPC alone cannot unblock a local pipe writer.
		s.closeErr = errors.Join(s.stdout.Close(), s.stderr.Close(), s.access.close())
	})
	return s.closeErr
}

type agentConn struct {
	reader         net.Conn
	writer         net.Conn
	responseBridge net.Conn
	requestBridge  net.Conn
	access         *agentAccessStream
	once           sync.Once
	mu             sync.Mutex
	terminalErr    error
	outputEOF      bool
}

func (b *agentBackend) openStream(ctx context.Context, runtimeUUID string, capability agentv1.AccessCapability, endpointID string, open *agentv1.OpenAccess) (*agentConn, error) {
	access, err := b.open(ctx, runtimeUUID, capability, endpointID, open)
	if err != nil {
		return nil, err
	}
	reader, responseBridge := net.Pipe()
	writer, requestBridge := net.Pipe()
	conn := &agentConn{reader: reader, writer: writer, responseBridge: responseBridge, requestBridge: requestBridge, access: access}
	stopClose := context.AfterFunc(access.ctx, func() { conn.finish(access.ctx.Err()) })
	go func() {
		defer stopClose()
		var diagnostic []byte
		var receiveErr error
		defer func() {
			if receiveErr != nil && len(diagnostic) != 0 {
				receiveErr = fmt.Errorf("%w: %s", receiveErr, strings.TrimSpace(string(diagnostic)))
			}
			conn.finish(receiveErr)
		}()
		outputEOF := false
		for {
			response, err := access.stream.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				receiveErr = err
				return
			}
			switch frame := response.Frame.(type) {
			case *agentv1.AccessResponse_Stdout:
				if outputEOF {
					err = fmt.Errorf("agent sent data after TCP output EOF")
				} else {
					_, err = responseBridge.Write(frame.Stdout)
				}
			case *agentv1.AccessResponse_Stderr:
				diagnostic = append(diagnostic, frame.Stderr[:min(len(frame.Stderr), 4096-len(diagnostic))]...)
			case *agentv1.AccessResponse_StdoutEof:
				if outputEOF || !frame.StdoutEof || capability == agentv1.AccessCapability_ACCESS_CAPABILITY_SFTP {
					err = fmt.Errorf("invalid Agent TCP output EOF")
				} else {
					outputEOF = true
					conn.mu.Lock()
					conn.outputEOF = true
					conn.mu.Unlock()
					_ = responseBridge.Close()
				}
			case *agentv1.AccessResponse_ExitStatus:
				if frame.ExitStatus != 0 {
					receiveErr = &workspaceCommandExitError{Status: int(frame.ExitStatus)}
				} else if !outputEOF && capability != agentv1.AccessCapability_ACCESS_CAPABILITY_SFTP {
					receiveErr = io.ErrUnexpectedEOF
				}
				return
			default:
				err = fmt.Errorf("invalid Agent output frame")
			}
			if err != nil {
				receiveErr = err
				return
			}
		}
	}()
	go func() {
		input := &agentInput{access: access}
		_, err := io.Copy(input, requestBridge)
		if err == nil {
			err = input.Close()
		}
		if err != nil {
			conn.finish(err)
		}
		_ = requestBridge.Close()
	}()
	return conn, nil
}

func (b *agentBackend) OpenWorkspaceSFTP(ctx context.Context, request workspaceSFTPRequest) (io.ReadWriteCloser, error) {
	return b.openStream(ctx, request.RuntimeUUID, agentv1.AccessCapability_ACCESS_CAPABILITY_SFTP, "", &agentv1.OpenAccess{Capability: &agentv1.OpenAccess_Sftp{Sftp: &agentv1.SFTP{}}})
}

func (b *agentBackend) OpenWorkspaceTCP(ctx context.Context, runtimeUUID string, port uint32) (net.Conn, error) {
	return b.openStream(ctx, runtimeUUID, agentv1.AccessCapability_ACCESS_CAPABILITY_LOOPBACK_TCP, "", &agentv1.OpenAccess{Capability: &agentv1.OpenAccess_Tcp{Tcp: &agentv1.LoopbackTCP{Port: port}}})
}

func (b *agentBackend) OpenWorkspaceEndpoint(ctx context.Context, runtimeUUID, endpointID string) (net.Conn, error) {
	return b.openStream(ctx, runtimeUUID, agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT, endpointID, &agentv1.OpenAccess{Capability: &agentv1.OpenAccess_Endpoint{Endpoint: &agentv1.Endpoint{EndpointId: endpointID}}})
}

func (c *agentConn) Read(value []byte) (int, error) {
	n, err := c.reader.Read(value)
	if err != nil {
		c.mu.Lock()
		if !c.outputEOF && c.terminalErr != nil {
			err = c.terminalErr
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *agentConn) Write(value []byte) (int, error) {
	n, err := c.writer.Write(value)
	if err != nil {
		c.mu.Lock()
		if c.terminalErr != nil {
			err = c.terminalErr
		}
		c.mu.Unlock()
	}
	return n, err
}
func (c *agentConn) CloseWrite() error { return c.writer.Close() }

func (c *agentConn) finish(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.terminalErr = err
		c.mu.Unlock()
		_ = c.responseBridge.Close()
		_ = c.requestBridge.Close()
		_ = c.access.close()
	})
}

func (c *agentConn) Close() error {
	c.finish(net.ErrClosed)
	return errors.Join(c.reader.Close(), c.writer.Close())
}
func (c *agentConn) LocalAddr() net.Addr  { return agentAddress("gateway") }
func (c *agentConn) RemoteAddr() net.Addr { return agentAddress("runtime-agent") }

func (c *agentConn) SetDeadline(deadline time.Time) error {
	return errors.Join(c.SetReadDeadline(deadline), c.SetWriteDeadline(deadline))
}

func (c *agentConn) SetReadDeadline(deadline time.Time) error {
	return c.reader.SetReadDeadline(deadline)
}

func (c *agentConn) SetWriteDeadline(deadline time.Time) error {
	return c.writer.SetWriteDeadline(deadline)
}

type agentAddress string

func (a agentAddress) Network() string { return "agent-rpc" }
func (a agentAddress) String() string  { return string(a) }

var _ workspaceBackend = (*agentBackend)(nil)
var _ net.Conn = (*agentConn)(nil)

func validAgentAddress(value string) bool {
	host, port, err := net.SplitHostPort(value)
	return err == nil && strings.TrimSpace(host) != "" && port == "8444"
}
