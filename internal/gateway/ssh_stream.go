// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"io"
	"time"

	"golang.org/x/crypto/ssh"
)

func (s *SSHServer) serveWorkspaceSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, session WorkspaceCommandSession, pty gatewaySSHPty, activity chan<- struct{}) {
	defer func() { _ = session.Close() }()
	if pty.enabled {
		_ = session.Resize(pty.cols, pty.rows)
	}
	stdinDone := make(chan struct{})
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	waitDone := make(chan error, 1)
	requestsDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		_ = copyGatewaySSHData(session.Stdin(), channel, activity)
		_ = session.Stdin().Close()
	}()
	go func() {
		defer close(stdoutDone)
		_ = copyGatewaySSHData(channel, session.Stdout(), activity)
	}()
	go func() {
		defer close(stderrDone)
		_ = copyGatewaySSHData(channel.Stderr(), session.Stderr(), activity)
	}()
	go func() {
		waitDone <- session.Wait()
	}()
	go func() {
		defer close(requestsDone)
		s.handleWorkspaceSessionRequests(session, requests, pty.enabled, activity)
	}()

	select {
	case <-ctx.Done():
	case err := <-waitDone:
		waitGatewaySSHChannels(ctx, stdoutDone, stderrDone)
		status := gatewaySSHExitStatus(err)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
	case <-requestsDone:
	}
	_ = channel.Close()
	_ = session.Close()
	waitGatewaySSHChannels(ctx, stdinDone, stdoutDone, stderrDone)
}

func (s *SSHServer) proxyWorkspaceStream(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, conn io.ReadWriteCloser, activity chan<- struct{}) {
	requestsDone := make(chan struct{})
	go func() {
		defer close(requestsDone)
		s.rejectWorkspaceSessionRequests(requests, activity)
	}()
	clientToBackendDone := make(chan struct{})
	backendToClientDone := make(chan struct{})
	go func() {
		defer close(clientToBackendDone)
		_ = copyGatewaySSHData(conn, channel, activity)
		if writer, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = writer.CloseWrite()
		}
	}()
	go func() {
		defer close(backendToClientDone)
		_ = copyGatewaySSHData(channel, conn, activity)
	}()
	select {
	case <-ctx.Done():
	case <-requestsDone:
	case <-backendToClientDone:
	case <-clientToBackendDone:
		select {
		case <-ctx.Done():
		case <-requestsDone:
		case <-backendToClientDone:
		}
	}
	_ = channel.Close()
	_ = conn.Close()
	waitGatewaySSHChannels(ctx, clientToBackendDone, backendToClientDone)
}

func (s *SSHServer) handleWorkspaceSessionRequests(session WorkspaceCommandSession, requests <-chan *ssh.Request, ptyEnabled bool, activity chan<- struct{}) {
	for request := range requests {
		notifyGatewaySSHActivity(activity)
		switch request.Type {
		case "window-change":
			if ptyEnabled {
				pty := parseGatewaySSHWindowChange(request.Payload, gatewaySSHPty{})
				_ = session.Resize(pty.cols, pty.rows)
			}
		case "signal":
			signal, ok := parseGatewaySSHSignal(request.Payload)
			if ok {
				ok = session.Signal(signal) == nil
			}
			if request.WantReply {
				_ = request.Reply(ok, nil)
			}
		default:
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
		}
	}
}

func (s *SSHServer) rejectWorkspaceSessionRequests(requests <-chan *ssh.Request, activity chan<- struct{}) {
	for request := range requests {
		notifyGatewaySSHActivity(activity)
		if request.WantReply {
			_ = request.Reply(false, nil)
		}
	}
}

func waitGatewaySSHChannels(ctx context.Context, channels ...<-chan struct{}) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for _, done := range channels {
		select {
		case <-done:
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		}
	}
}

func copyGatewaySSHData(dst io.Writer, src io.Reader, activity chan<- struct{}) error {
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buffer)
		if n > 0 {
			notifyGatewaySSHActivity(activity)
			written, writeErr := dst.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}
