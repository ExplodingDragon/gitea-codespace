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
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"gitea.dev/codespace/internal/rpc/component/v1/componentv1connect"
	"gitea.dev/codespace/internal/transport"
)

// AgentBackend adapts Gateway sessions to the current Runtime Agent. It asks
// Manager for a fresh target-bound ticket for every new data stream.
type AgentBackend struct {
	Manager              componentv1connect.ComponentServiceClient
	CertificateDirectory string

	mu      sync.Mutex
	clients map[string]*agentClient
}

type agentClient struct {
	address     string
	siteUID     string
	resourceUID string
	transport   *http.Transport
	access      agentv1connect.AgentAccessServiceClient
}

type agentAccessStream struct {
	cancel context.CancelFunc
	stream *connect.BidiStreamForClient[agentv1.AccessRequest, agentv1.AccessResponse]
	mu     sync.Mutex
}

func (b *AgentBackend) open(ctx context.Context, runtimeUUID string, capability agentv1.AccessCapability, endpointID string, open *agentv1.OpenAccess) (*agentAccessStream, error) {
	if b == nil || b.Manager == nil {
		return nil, fmt.Errorf("agent backend is unavailable")
	}
	issueCtx, cancelIssue := context.WithTimeout(ctx, 10*time.Second)
	response, err := b.Manager.IssueAgentAccess(issueCtx, connect.NewRequest(&componentv1.IssueAgentAccessRequest{ProtocolVersion: 1, RuntimeUuid: runtimeUUID, Capability: capability, EndpointId: endpointID}))
	cancelIssue()
	if err != nil {
		return nil, err
	}
	route := response.Msg.Runtime
	if route == nil || route.RuntimeUuid != runtimeUUID || route.SiteUid == "" || route.ResourceUid == "" || route.PodUid == "" || route.AgentAddress == "" || response.Msg.Ticket == "" {
		return nil, fmt.Errorf("manager returned an incomplete Agent target")
	}
	client, err := b.clientFor(route)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream := client.access.Access(streamCtx)
	open.Ticket = response.Msg.Ticket
	if err := stream.Send(&agentv1.AccessRequest{ProtocolVersion: 1, Frame: &agentv1.AccessRequest_Open{Open: open}}); err != nil {
		cancel()
		return nil, err
	}
	accepted, err := stream.Receive()
	if err != nil || !accepted.GetAccepted() {
		cancel()
		if err == nil {
			err = fmt.Errorf("agent rejected the access stream")
		}
		return nil, err
	}
	return &agentAccessStream{cancel: cancel, stream: stream}, nil
}

func (b *AgentBackend) clientFor(route *componentv1.GatewayRuntimeRoute) (*agentClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.clients == nil {
		b.clients = make(map[string]*agentClient)
	}
	if client := b.clients[route.PodUid]; client != nil {
		if client.address == route.AgentAddress && client.siteUID == route.SiteUid && client.resourceUID == route.ResourceUid {
			return client, nil
		}
		client.transport.CloseIdleConnections()
		delete(b.clients, route.PodUid)
	}
	directory := b.CertificateDirectory
	if directory == "" {
		directory = "/var/run/codespace/identity"
	}
	expected := "spiffe://codespace/agent/" + route.SiteUid + "/" + route.ResourceUid + "/" + route.PodUid
	tlsConfig, err := (transport.Certificates{Directory: directory}).Client(func(identity *url.URL) error {
		if identity.String() != expected {
			return fmt.Errorf("agent identity does not match the current Runtime Pod")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	httpTransport := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second}
	httpClient := &http.Client{Transport: httpTransport}
	client := &agentClient{
		address: route.AgentAddress, siteUID: route.SiteUid, resourceUID: route.ResourceUid,
		transport: httpTransport,
		access:    agentv1connect.NewAgentAccessServiceClient(httpClient, "https://"+route.AgentAddress, connect.WithGRPC()),
	}
	b.clients[route.PodUid] = client
	return client, nil
}

func (b *AgentBackend) ClosePod(podUID string) {
	b.mu.Lock()
	client := b.clients[podUID]
	delete(b.clients, podUID)
	b.mu.Unlock()
	if client != nil {
		client.transport.CloseIdleConnections()
	}
}

func (b *AgentBackend) Close() {
	b.mu.Lock()
	clients := b.clients
	b.clients = make(map[string]*agentClient)
	b.mu.Unlock()
	for _, client := range clients {
		client.transport.CloseIdleConnections()
	}
}

func (s *agentAccessStream) send(frame *agentv1.AccessRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(frame)
}

func (s *agentAccessStream) close() error {
	s.cancel()
	requestErr := s.stream.CloseRequest()
	responseErr := s.stream.CloseResponse()
	if errors.Is(requestErr, io.EOF) || errors.Is(requestErr, context.Canceled) || errors.Is(requestErr, net.ErrClosed) {
		requestErr = nil
	}
	if errors.Is(responseErr, io.EOF) || errors.Is(responseErr, context.Canceled) || errors.Is(responseErr, net.ErrClosed) {
		responseErr = nil
	}
	return errors.Join(requestErr, responseErr)
}

type agentCommandSession struct {
	access *agentAccessStream
	stdin  *agentInput
	stdout *io.PipeReader
	stderr *io.PipeReader
	done   chan error
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

func (b *AgentBackend) OpenWorkspaceCommand(ctx context.Context, request WorkspaceCommandRequest) (WorkspaceCommandSession, error) {
	capability := agentv1.AccessCapability_ACCESS_CAPABILITY_COMMAND
	command := &agentv1.Command{Command: request.Command}
	open := &agentv1.OpenAccess{Capability: &agentv1.OpenAccess_Command{Command: command}}
	if request.Interactive {
		terminal := &agentv1.Terminal{Term: "xterm-256color", Size: &agentv1.TerminalSize{Columns: uint32(max(request.Cols, 1)), Rows: uint32(max(request.Rows, 1))}}
		if request.Command == "" {
			capability = agentv1.AccessCapability_ACCESS_CAPABILITY_TERMINAL
			open.Capability = &agentv1.OpenAccess_Terminal{Terminal: terminal}
		} else {
			command.Terminal = terminal
		}
	}
	access, err := b.open(ctx, request.RuntimeUUID, capability, "", open)
	if err != nil {
		return nil, err
	}
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	session := &agentCommandSession{access: access, stdout: stdoutReader, stderr: stderrReader, done: make(chan error, 1)}
	session.stdin = &agentInput{access: access}
	go func() {
		defer func() { _ = stdoutWriter.Close() }()
		defer func() { _ = stderrWriter.Close() }()
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
					session.done <- &WorkspaceCommandExitError{Status: int(frame.ExitStatus)}
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
func (s *agentCommandSession) Wait() error  { return <-s.done }
func (s *agentCommandSession) Close() error { return s.access.close() }

type agentConn struct {
	reader net.Conn
	writer net.Conn
	access *agentAccessStream
	once   sync.Once
}

func (b *AgentBackend) openStream(ctx context.Context, runtimeUUID string, capability agentv1.AccessCapability, endpointID string, open *agentv1.OpenAccess) (*agentConn, error) {
	access, err := b.open(ctx, runtimeUUID, capability, endpointID, open)
	if err != nil {
		return nil, err
	}
	reader, responseBridge := net.Pipe()
	writer, requestBridge := net.Pipe()
	conn := &agentConn{reader: reader, writer: writer, access: access}
	go func() {
		defer func() { _ = responseBridge.Close() }()
		defer func() { _ = requestBridge.Close() }()
		defer access.cancel()
		for {
			response, err := access.stream.Receive()
			if err != nil {
				return
			}
			switch frame := response.Frame.(type) {
			case *agentv1.AccessResponse_Stdout:
				_, err = responseBridge.Write(frame.Stdout)
			case *agentv1.AccessResponse_Stderr:
				_, err = responseBridge.Write(frame.Stderr)
			case *agentv1.AccessResponse_ExitStatus:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		input := &agentInput{access: access}
		_, _ = io.Copy(input, requestBridge)
		_ = input.Close()
		_ = requestBridge.Close()
	}()
	return conn, nil
}

func (b *AgentBackend) OpenWorkspaceSFTP(ctx context.Context, request WorkspaceSFTPRequest) (io.ReadWriteCloser, error) {
	return b.openStream(ctx, request.RuntimeUUID, agentv1.AccessCapability_ACCESS_CAPABILITY_SFTP, "", &agentv1.OpenAccess{Capability: &agentv1.OpenAccess_Sftp{Sftp: &agentv1.SFTP{}}})
}

func (b *AgentBackend) OpenWorkspaceTCP(ctx context.Context, runtimeUUID string, port uint32) (net.Conn, error) {
	return b.openStream(ctx, runtimeUUID, agentv1.AccessCapability_ACCESS_CAPABILITY_LOOPBACK_TCP, "", &agentv1.OpenAccess{Capability: &agentv1.OpenAccess_Tcp{Tcp: &agentv1.LoopbackTCP{Port: port}}})
}

func (b *AgentBackend) OpenWorkspaceEndpoint(ctx context.Context, runtimeUUID, endpointID string) (net.Conn, error) {
	return b.openStream(ctx, runtimeUUID, agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT, endpointID, &agentv1.OpenAccess{Capability: &agentv1.OpenAccess_Endpoint{Endpoint: &agentv1.Endpoint{EndpointId: endpointID}}})
}

func (c *agentConn) Read(value []byte) (int, error)  { return c.reader.Read(value) }
func (c *agentConn) Write(value []byte) (int, error) { return c.writer.Write(value) }
func (c *agentConn) CloseWrite() error               { return c.writer.Close() }

func (c *agentConn) Close() error {
	var err error
	c.once.Do(func() {
		err = errors.Join(c.reader.Close(), c.writer.Close(), c.access.close())
	})
	return err
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

var _ WorkspaceBackend = (*AgentBackend)(nil)
var _ net.Conn = (*agentConn)(nil)

func validAgentAddress(value string) bool {
	host, port, err := net.SplitHostPort(value)
	return err == nil && strings.TrimSpace(host) != "" && port == "8444"
}
