// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

type streamTestChannel struct {
	input    *io.PipeReader
	output   bytes.Buffer
	stderr   bytes.Buffer
	requests chan *ssh.Request
	once     sync.Once
	status   uint32
}

func (c *streamTestChannel) Read(p []byte) (int, error)  { return c.input.Read(p) }
func (c *streamTestChannel) Write(p []byte) (int, error) { return c.output.Write(p) }
func (c *streamTestChannel) Stderr() io.ReadWriter       { return &c.stderr }
func (c *streamTestChannel) CloseWrite() error           { return nil }
func (c *streamTestChannel) Close() error {
	c.once.Do(func() {
		_ = c.input.Close()
		close(c.requests)
	})
	return nil
}
func (c *streamTestChannel) SendRequest(name string, _ bool, payload []byte) (bool, error) {
	var exit struct{ Status uint32 }
	err := ssh.Unmarshal(payload, &exit)
	c.status = exit.Status
	return name == "exit-status", err
}

type streamTestSession struct{ io.WriteCloser }

func (s streamTestSession) Stdin() io.WriteCloser { return s.WriteCloser }
func (streamTestSession) Stdout() io.Reader       { return strings.NewReader("output") }
func (streamTestSession) Stderr() io.Reader       { return strings.NewReader("error") }
func (streamTestSession) Resize(int, int) error   { return nil }
func (streamTestSession) Signal(int) error        { return nil }
func (streamTestSession) Wait() error             { return &WorkspaceCommandExitError{Status: 7} }

func TestSSHCommandDrainsOutputBeforeExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		input, writer := io.Pipe()
		defer writer.Close()
		channel := &streamTestChannel{input: input, requests: make(chan *ssh.Request)}
		defer channel.Close()
		done := make(chan struct{})
		go func() {
			defer close(done)
			(&SSHServer{}).serveWorkspaceSession(context.Background(), channel, channel.requests, streamTestSession{writer}, gatewaySSHPty{}, nil)
		}()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("session did not finish after draining output")
		}
		require.Equal(t, "output", channel.output.String())
		require.Equal(t, "error", channel.stderr.String())
		require.EqualValues(t, 7, channel.status)
	})
}
