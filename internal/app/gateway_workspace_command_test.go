// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/pkg/sftp"

	"gitea.dev/codespace/internal/provisioner"
)

type testWorkspaceCommandBackend struct {
	mu          sync.Mutex
	output      string
	exitStatus  int
	block       bool
	requests    []provisioner.WorkspaceCommandRequest
	resizes     []testWorkspaceResize
	openedShell chan struct{}
}

type testWorkspaceResize struct {
	cols int
	rows int
}

func newTestWorkspaceCommandBackend(output string) *testWorkspaceCommandBackend {
	return &testWorkspaceCommandBackend{
		output:      output,
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
	if request.Workdir == "" {
		return nil, fmt.Errorf("workdir is empty")
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
	go func() {
		_, _ = io.Copy(io.Discard, stdinReader)
	}()
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
	clientConn, serverConn := net.Pipe()
	server := sftp.NewRequestServer(serverConn, sftp.InMemHandler(), sftp.WithStartDirectory("/"))
	go func() {
		_ = server.Serve()
		_ = serverConn.Close()
	}()
	return clientConn, nil
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
	s.backend.mu.Lock()
	s.backend.resizes = append(s.backend.resizes, testWorkspaceResize{cols: cols, rows: rows})
	s.backend.mu.Unlock()
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
