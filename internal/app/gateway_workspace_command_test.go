// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/sftp"

	"gitea.dev/codespace/internal/provisioner"
)

func saveGatewayWorkspaceIdentityForTest(t testingT, store *CodespaceStateStore, codespaceUUID string) {
	t.Helper()
	if err := store.SaveScriptEnvironment(codespaceUUID, map[string]string{
		"CODESPACE_CREDENTIAL_UID":       "1000",
		"CODESPACE_CREDENTIAL_GID":       "1000",
		"CODESPACE_DEVCONTAINER_ID":      "container-1",
		"CODESPACE_DEVCONTAINER_USER":    "developer",
		"CODESPACE_DEVCONTAINER_WORKDIR": "/workspaces/repo",
		provisioner.WorkspaceIDEPortEnv:  strconv.Itoa(provisioner.WorkspaceIDEPort),
	}); err != nil {
		t.Fatalf("save script environment: %v", err)
	}
}

type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
}

type testWorkspaceCommandBackend struct {
	mu          sync.Mutex
	output      string
	exitStatus  int
	block       bool
	stdin       chan []byte
	resize      chan testWorkspaceResize
	signal      chan int
	requests    []provisioner.WorkspaceCommandRequest
	resizes     []testWorkspaceResize
	signals     []int
	openedShell chan struct{}
	tcpAddress  string
	tcpPorts    []uint32
}

type testWorkspaceResize struct {
	cols int
	rows int
}

func newTestWorkspaceCommandBackend(output string) *testWorkspaceCommandBackend {
	return &testWorkspaceCommandBackend{
		output:      output,
		resize:      make(chan testWorkspaceResize, 16),
		signal:      make(chan int, 16),
		openedShell: make(chan struct{}, 1),
	}
}

func (b *testWorkspaceCommandBackend) OpenWorkspaceCommand(ctx context.Context, request provisioner.WorkspaceCommandRequest) (provisioner.WorkspaceCommandSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.InstanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	if request.ContainerID == "" || request.ContainerUser == "" || request.ContainerWorkdir == "" {
		return nil, fmt.Errorf("Dev Container target is missing")
	}
	b.mu.Lock()
	b.requests = append(b.requests, request)
	block := b.block || request.Command == "sleep"
	output := b.output
	exitStatus := b.exitStatus
	b.mu.Unlock()

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	session := &testWorkspaceCommandSession{
		backend:     b,
		stdin:       stdinWriter,
		stdout:      stdoutReader,
		stderr:      stderrReader,
		stdoutClose: stdoutWriter,
		stderrClose: stderrWriter,
		waitDone:    make(chan error, 1),
		closeDone:   make(chan struct{}),
	}
	select {
	case b.openedShell <- struct{}{}:
	default:
	}
	go b.readStdin(stdinReader)
	go func() {
		if output != "" {
			_, _ = io.WriteString(stdoutWriter, output)
		}
		if block {
			<-session.closeDone
			_ = stdoutWriter.Close()
			_ = stderrWriter.Close()
			session.waitDone <- fmt.Errorf("workspace command closed")
			return
		}
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		if exitStatus != 0 {
			session.waitDone <- &provisioner.WorkspaceCommandExitError{Status: exitStatus}
			return
		}
		session.waitDone <- nil
	}()
	return session, nil
}

func (b *testWorkspaceCommandBackend) captureStdin() {
	b.stdin = make(chan []byte, 16)
}

func (b *testWorkspaceCommandBackend) readStdin(reader io.Reader) {
	if b.stdin == nil {
		_, _ = io.Copy(io.Discard, reader)
		return
	}
	buffer := make([]byte, 1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			payload := append([]byte(nil), buffer[:n]...)
			b.stdin <- payload
		}
		if err != nil {
			return
		}
	}
}

func (b *testWorkspaceCommandBackend) OpenWorkspaceSFTP(ctx context.Context, request provisioner.WorkspaceSFTPRequest) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.InstanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	if request.Workdir == "" {
		return nil, fmt.Errorf("workdir is empty")
	}
	if request.User == 0 || request.Group == 0 {
		return nil, fmt.Errorf("workspace user and group are required")
	}
	clientConn, serverConn := net.Pipe()
	server := sftp.NewRequestServer(serverConn, sftp.InMemHandler(), sftp.WithStartDirectory("/"))
	go func() {
		_ = server.Serve()
		_ = serverConn.Close()
	}()
	return clientConn, nil
}

func (b *testWorkspaceCommandBackend) OpenWorkspaceTCP(ctx context.Context, instanceName string, port uint32) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if instanceName == "" || port == 0 || port > 65535 {
		return nil, fmt.Errorf("workspace tcp target is invalid")
	}
	b.mu.Lock()
	b.tcpPorts = append(b.tcpPorts, port)
	address := b.tcpAddress
	b.mu.Unlock()
	if address == "" {
		address = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

func (b *testWorkspaceCommandBackend) CheckWorkspaceAccess(ctx context.Context, instanceName, workdir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" || workdir == "" {
		return fmt.Errorf("workspace target is missing")
	}
	return nil
}

func (b *testWorkspaceCommandBackend) CheckDevContainer(ctx context.Context, instanceName, containerID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" || containerID == "" {
		return fmt.Errorf("Dev Container target is missing")
	}
	return nil
}

func (b *testWorkspaceCommandBackend) lastRequest() provisioner.WorkspaceCommandRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		return provisioner.WorkspaceCommandRequest{}
	}
	return b.requests[len(b.requests)-1]
}

type testWorkspaceCommandSession struct {
	backend     *testWorkspaceCommandBackend
	stdin       *io.PipeWriter
	stdout      *io.PipeReader
	stderr      *io.PipeReader
	stdoutClose *io.PipeWriter
	stderrClose *io.PipeWriter
	waitDone    chan error
	closeDone   chan struct{}
	closeOnce   sync.Once
}

func (s *testWorkspaceCommandSession) Stdin() io.WriteCloser {
	return s.stdin
}

func (s *testWorkspaceCommandSession) Stdout() io.Reader {
	return s.stdout
}

func (s *testWorkspaceCommandSession) Stderr() io.Reader {
	return s.stderr
}

func (s *testWorkspaceCommandSession) Resize(cols, rows int) error {
	resize := testWorkspaceResize{cols: cols, rows: rows}
	s.backend.mu.Lock()
	s.backend.resizes = append(s.backend.resizes, resize)
	s.backend.mu.Unlock()
	select {
	case s.backend.resize <- resize:
	default:
	}
	return nil
}

func (s *testWorkspaceCommandSession) Signal(signal int) error {
	s.backend.mu.Lock()
	s.backend.signals = append(s.backend.signals, signal)
	s.backend.mu.Unlock()
	select {
	case s.backend.signal <- signal:
	default:
	}
	return nil
}

func (s *testWorkspaceCommandSession) Wait() error {
	return <-s.waitDone
}

func (s *testWorkspaceCommandSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.closeDone)
		_ = s.stdin.Close()
	})
	return nil
}

func waitTestWorkspaceResize(t testingT, backend *testWorkspaceCommandBackend, cols, rows int) testWorkspaceResize {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case resize := <-backend.resize:
			if resize.cols == cols && resize.rows == rows {
				return resize
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for terminal resize %dx%d", cols, rows)
		}
	}
}
