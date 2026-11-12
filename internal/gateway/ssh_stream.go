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

func (s *sshServer) serveWorkspaceSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, session workspaceCommandSession, pty gatewaySSHPty, activity chan<- struct{}) {
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

func (s *sshServer) proxyWorkspaceStream(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, conn io.ReadWriteCloser, activity chan<- struct{}) {
	// Request delivery can end while channel data is still buffered for copying.
	go s.rejectWorkspaceSessionRequests(requests, activity)
	copyDone := make(chan error, 2)
	go func() {
		err := copyGatewaySSHData(conn, channel, activity)
		if writer, ok := conn.(interface{ CloseWrite() error }); ok && err == nil {
			err = writer.CloseWrite()
		}
		copyDone <- err
	}()
	go func() {
		err := copyGatewaySSHData(channel, conn, activity)
		if err == nil {
			err = channel.CloseWrite()
		}
		copyDone <- err
	}()
	remaining := 2
copying:
	for remaining > 0 {
		select {
		case <-ctx.Done():
			break copying
		case err := <-copyDone:
			remaining--
			if err != nil {
				break copying
			}
		}
	}
	_ = channel.Close()
	_ = conn.Close()
	for range remaining {
		<-copyDone
	}
}

func (s *sshServer) handleWorkspaceSessionRequests(session workspaceCommandSession, requests <-chan *ssh.Request, ptyEnabled bool, activity chan<- struct{}) {
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

func (s *sshServer) rejectWorkspaceSessionRequests(requests <-chan *ssh.Request, activity chan<- struct{}) {
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
