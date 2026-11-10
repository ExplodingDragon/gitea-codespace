// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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

type gatewaySiteTargetStore struct {
	siteUID string
}

func (s gatewaySiteTargetStore) RuntimeTarget(runtimeUUID string) (runtimeSnapshot, bool) {
	return runtimeSnapshot{SiteUID: s.siteUID, RuntimeUUID: runtimeUUID}, true
}

func (s streamTestSession) Stdin() io.WriteCloser { return s.WriteCloser }
func (streamTestSession) Stdout() io.Reader       { return strings.NewReader("output") }
func (streamTestSession) Stderr() io.Reader       { return strings.NewReader("error") }
func (streamTestSession) Resize(int, int) error   { return nil }
func (streamTestSession) Signal(int) error        { return nil }
func (streamTestSession) Wait() error             { return &workspaceCommandExitError{Status: 7} }

func TestSSHCommandDrainsOutputBeforeExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		input, writer := io.Pipe()
		defer func() { _ = writer.Close() }()
		channel := &streamTestChannel{input: input, requests: make(chan *ssh.Request)}
		defer func() { _ = channel.Close() }()
		done := make(chan struct{})
		go func() {
			defer close(done)
			(&sshServer{}).serveWorkspaceSession(context.Background(), channel, channel.requests, streamTestSession{writer}, gatewaySSHPty{}, nil)
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

func TestGatewaySSHRuntimeRequiresAuthorizedSite(t *testing.T) {
	server := &sshServer{state: gatewaySiteTargetStore{siteUID: "site-1"}, backend: gatewayTestBackend{}}
	available := server.runtimeAvailable("runtime", "site-1")
	require.True(t, available)
	available = server.runtimeAvailable("runtime", "site-2")
	require.False(t, available)
}

func TestSSHForwardingHalfCloseAndChannelIsolation(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	targets, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer func() { _ = targets.Close() }()
	require.NoError(t, targets.SetDeadline(time.Now().Add(5*time.Second)))
	done := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		server, channels, requests, err := ssh.NewServerConn(raw, config)
		if err != nil {
			_ = raw.Close()
			done <- err
			return
		}
		defer func() { _ = server.Close() }()
		go ssh.DiscardRequests(requests)
		var workers sync.WaitGroup
		for incoming := range channels {
			conn, err := net.DialTimeout("tcp", targets.Addr().String(), time.Second)
			if err != nil {
				done <- err
				return
			}
			channel, requests, err := incoming.Accept()
			if err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			workers.Go(func() {
				(&sshServer{}).proxyWorkspaceStream(t.Context(), channel, requests, conn, nil)
			})
		}
		workers.Wait()
		done <- nil
	}()
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User: "test", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	channels := make([]ssh.Channel, 0, 2)
	connections := make([]*net.TCPConn, 0, 2)
	for range 2 {
		channel, requests, err := client.OpenChannel("direct-tcpip", nil)
		require.NoError(t, err)
		go ssh.DiscardRequests(requests)
		channels = append(channels, channel)
		conn, err := targets.AcceptTCP()
		require.NoError(t, err)
		require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
		connections = append(connections, conn)
		defer func() { _ = conn.Close() }()
	}
	// The target finishes sending first; the client must still be able to send.
	_, err = connections[0].Write([]byte("response"))
	require.NoError(t, err)
	require.NoError(t, connections[0].CloseWrite())
	response, err := io.ReadAll(channels[0])
	require.NoError(t, err)
	require.Equal(t, "response", string(response))
	_, err = channels[0].Write([]byte("final input"))
	require.NoError(t, err)
	require.NoError(t, channels[0].CloseWrite())
	input, err := io.ReadAll(connections[0])
	require.NoError(t, err)
	require.Equal(t, "final input", string(input))
	_ = channels[0].Close()
	// The second channel remains usable and closes its input first.
	_, err = channels[1].Write([]byte("second request"))
	require.NoError(t, err)
	require.NoError(t, channels[1].CloseWrite())
	input, err = io.ReadAll(connections[1])
	require.NoError(t, err)
	require.Equal(t, "second request", string(input))
	_, err = connections[1].Write([]byte("second response"))
	require.NoError(t, err)
	require.NoError(t, connections[1].CloseWrite())
	response, err = io.ReadAll(channels[1])
	require.NoError(t, err)
	require.Equal(t, "second response", string(response))
	_ = channels[1].Close()
	require.NoError(t, client.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("SSH forwarding workers did not finish")
	}
}
