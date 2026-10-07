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
	stdinDone := make(chan struct{}, 1)
	stdoutDone := make(chan struct{}, 1)
	stderrDone := make(chan struct{}, 1)
	waitDone := make(chan error, 1)
	requestsDone := make(chan struct{}, 1)
	go func() {
		_ = copyGatewaySSHData(session.Stdin(), channel, activity)
		_ = session.Stdin().Close()
		stdinDone <- struct{}{}
	}()
	go func() {
		_ = copyGatewaySSHData(channel, session.Stdout(), activity)
		stdoutDone <- struct{}{}
	}()
	go func() {
		_ = copyGatewaySSHData(channel.Stderr(), session.Stderr(), activity)
		stderrDone <- struct{}{}
	}()
	go func() {
		waitDone <- session.Wait()
	}()
	go func() {
		s.handleWorkspaceSessionRequests(session, requests, pty.enabled, activity)
		requestsDone <- struct{}{}
	}()

	select {
	case <-ctx.Done():
	case err := <-waitDone:
		waitGatewaySSHChannelDone(stdoutDone)
		waitGatewaySSHChannelDone(stderrDone)
		status := gatewaySSHExitStatus(err)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
	case <-requestsDone:
	}
	_ = channel.Close()
	waitGatewaySSHChannelDone(stdinDone)
	waitGatewaySSHChannelDone(stdoutDone)
	waitGatewaySSHChannelDone(stderrDone)
}

func (s *SSHServer) proxyWorkspaceStream(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, conn io.ReadWriteCloser, activity chan<- struct{}) {
	requestsDone := make(chan struct{}, 1)
	go func() {
		s.rejectWorkspaceSessionRequests(requests, activity)
		requestsDone <- struct{}{}
	}()
	clientToBackendDone := make(chan struct{}, 1)
	backendToClientDone := make(chan struct{}, 1)
	go func() {
		_ = copyGatewaySSHData(conn, channel, activity)
		if writer, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = writer.CloseWrite()
		}
		clientToBackendDone <- struct{}{}
	}()
	go func() {
		_ = copyGatewaySSHData(channel, conn, activity)
		backendToClientDone <- struct{}{}
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
	waitGatewaySSHChannelDone(clientToBackendDone)
	waitGatewaySSHChannelDone(backendToClientDone)
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

func waitGatewaySSHChannelDone(done <-chan struct{}) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
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
