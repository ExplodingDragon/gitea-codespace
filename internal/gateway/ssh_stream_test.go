// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
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

type reverseTestListener struct {
	connections chan *workspaceReverseConnection
	failures    chan error
	closed      chan struct{}
	once        sync.Once
}

func (l *reverseTestListener) Port() uint32 { return 3240 }

func (l *reverseTestListener) Accept(ctx context.Context) (*workspaceReverseConnection, error) {
	select {
	case connection := <-l.connections:
		return connection, nil
	case err := <-l.failures:
		return nil, err
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *reverseTestListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
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

func TestGatewaySSHRevalidationPreservesSessionUntilDenied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var calls atomic.Int32
		server := &sshServer{
			revalidateInterval: time.Minute,
			controlPlane: gatewayHandlerTestControlPlane{revalidateSSH: func(context.Context) (gatewayAccessDecision, error) {
				if calls.Add(1) == 1 {
					return gatewayAccessDecision{}, errors.New("temporary failure")
				}
				return gatewayAccessDecision{DeniedCategory: "permission_denied"}, nil
			}},
		}
		go server.revalidateSession(ctx, gatewaySSHAuthContext{}, cancel)
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		require.EqualValues(t, 1, calls.Load())
		require.NoError(t, ctx.Err())
		time.Sleep(time.Minute)
		synctest.Wait()
		require.EqualValues(t, 2, calls.Load())
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})
}

func TestGatewayReverseForwardCloseWaitsForOpenChannel(t *testing.T) {
	reverse := &reverseTestListener{connections: make(chan *workspaceReverseConnection), closed: make(chan struct{})}
	forward := &gatewayReverseForward{listener: reverse}
	require.True(t, forward.beginOpen())

	closed := make(chan struct{})
	go func() {
		forward.close()
		close(closed)
	}()
	select {
	case <-reverse.closed:
	case <-time.After(time.Second):
		t.Fatal("Agent reverse listener was not closed")
	}
	select {
	case <-closed:
		t.Fatal("forward closed before the in-flight channel open finished")
	default:
	}
	require.False(t, forward.beginOpen())
	forward.endOpen()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("forward did not close after the in-flight channel open finished")
	}
	select {
	case <-reverse.closed:
	default:
		t.Fatal("Agent reverse listener was not closed")
	}
}

func dialReverseTestSSH(t *testing.T, backend workspaceBackend) (*ssh.Client, <-chan error) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	transport, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = transport.Close() })
	server := &sshServer{state: gatewaySiteTargetStore{siteUID: "site"}, backend: backend, maxChannels: 4}
	serverDone := make(chan error, 1)
	go func() {
		raw, err := transport.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		connection, channels, requests, err := ssh.NewServerConn(raw, config)
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()
		go func() {
			for channel := range channels {
				_ = channel.Reject(ssh.UnknownChannelType, "unsupported")
			}
		}()
		server.handleGlobalRequests(t.Context(), gatewaySSHAuthContext{codespaceUUID: "runtime", siteUID: "site"}, connection, requests, make(chan struct{}, 1), make(chan struct{}, 4))
		serverDone <- nil
	}()
	client, err := ssh.Dial("tcp", transport.Addr().String(), &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client, serverDone
}

func TestSSHReverseForwardUsesRuntimeListener(t *testing.T) {
	reverse := &reverseTestListener{connections: make(chan *workspaceReverseConnection, 1), closed: make(chan struct{})}
	client, serverDone := dialReverseTestSSH(t, gatewayTestBackend{reverse: reverse})
	forwarded, err := client.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	backend, peer := net.Pipe()
	reverse.connections <- &workspaceReverseConnection{Connection: backend, OriginHost: "127.0.0.1", OriginPort: 41000}
	connection, err := forwarded.Accept()
	require.NoError(t, err)
	defer connection.Close()
	defer peer.Close()
	_, err = connection.Write([]byte("usbip"))
	require.NoError(t, err)
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(3*time.Second)))
	buffer := make([]byte, 5)
	_, err = io.ReadFull(peer, buffer)
	require.NoError(t, err)
	require.Equal(t, "usbip", string(buffer))
	require.NoError(t, forwarded.Close())
	select {
	case <-reverse.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("reverse listener was not canceled")
	}
	require.NoError(t, client.Close())
	select {
	case err := <-serverDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("SSH server did not stop")
	}
}

func TestSSHReverseForwardAcceptFailureClosesSession(t *testing.T) {
	reverse := &reverseTestListener{connections: make(chan *workspaceReverseConnection), failures: make(chan error, 1), closed: make(chan struct{})}
	client, serverDone := dialReverseTestSSH(t, gatewayTestBackend{reverse: reverse})
	defer client.Close()
	_, err := client.Listen("tcp", "127.0.0.1:3240")
	require.NoError(t, err)
	reverse.failures <- errors.New("agent listener failed")
	select {
	case err := <-serverDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("SSH server did not close after reverse listener failure")
	}
	_, err = client.Listen("tcp", "127.0.0.1:3240")
	require.Error(t, err)
}

func TestSSHReverseForwardNamedSources(t *testing.T) {
	listeners := map[string]*reverseTestListener{}
	client, _ := dialReverseTestSSH(t, gatewayTestBackend{reverseOpen: func(address string, port uint32) (workspaceReverseListener, error) {
		listener := &reverseTestListener{connections: make(chan *workspaceReverseConnection, 1), closed: make(chan struct{})}
		listeners[address] = listener
		return listener, nil
	}})
	channels := client.HandleChannelOpen("forwarded-tcpip")
	for _, address := range []string{"usbip.workstation", "usbip.lab"} {
		ok, _, err := client.SendRequest("tcpip-forward", true, ssh.Marshal(gatewayTCPIPForwardRequest{Address: address, Port: 3240}))
		require.NoError(t, err)
		require.True(t, ok)
		connection, peer := net.Pipe()
		defer peer.Close()
		listeners[address].connections <- &workspaceReverseConnection{Connection: connection, OriginHost: "127.0.0.1", OriginPort: 41000}
		select {
		case channel := <-channels:
			var payload struct {
				Address    string
				Port       uint32
				Origin     string
				OriginPort uint32
			}
			require.NoError(t, ssh.Unmarshal(channel.ExtraData(), &payload))
			require.Equal(t, address, payload.Address)
			require.EqualValues(t, 3240, payload.Port)
			require.NoError(t, channel.Reject(ssh.ConnectionFailed, "fixture"))
		case <-time.After(3 * time.Second):
			t.Fatal("missing named forwarded channel")
		}
	}
	ok, _, err := client.SendRequest("cancel-tcpip-forward", true, ssh.Marshal(gatewayTCPIPForwardRequest{Address: "usbip.workstation", Port: 3240}))
	require.NoError(t, err)
	require.True(t, ok)
	select {
	case <-listeners["usbip.workstation"].closed:
	default:
		t.Fatal("source listener still open")
	}
	select {
	case <-listeners["usbip.lab"].closed:
		t.Fatal("unrelated source closed")
	default:
	}
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
