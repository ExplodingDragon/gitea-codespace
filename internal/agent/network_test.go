// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/stretchr/testify/require"
)

func TestDialLoopback(t *testing.T) {
	for _, address := range []struct{ network, host string }{{"tcp4", "127.0.0.1:0"}, {"tcp6", "[::1]:0"}} {
		t.Run(address.network, func(t *testing.T) {
			listener, err := net.Listen(address.network, address.host)
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })
			connection, err := dialLoopback(t.Context(), uint32(listener.Addr().(*net.TCPAddr).Port))
			require.NoError(t, err)
			t.Cleanup(func() { _ = connection.Close() })
			target, err := listener.Accept()
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.Equal(t, listener.Addr().String(), connection.RemoteAddr().String())
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := dialLoopback(ctx, 1)
	require.ErrorIs(t, err, context.Canceled)
}

func TestNetworkAccessLifecycle(t *testing.T) {
	for _, action := range []string{"stop", "replace", "cancel"} {
		t.Run(action, func(t *testing.T) {
			runtime := &Runtime{}
			target := &agentv1.AccessTarget{PrimaryContainerId: "primary", Version: 1}
			runtime.setAccessState(target, nil)
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, release, err := runtime.networkAccessContext(caller, target)
			require.NoError(t, err)
			defer release()
			client, server := tcpTestConnections(t)
			require.NoError(t, client.SetDeadline(time.Now().Add(3*time.Second)))
			stream := &fakeTCPAccessStream{requests: make(chan *agentv1.AccessRequest, 1), responses: make(chan *agentv1.AccessResponse, 4)}
			defer close(stream.requests)
			done := make(chan error, 1)
			go func() { done <- bridgeTCPConnection(ctx, stream, server) }()
			require.True(t, (<-stream.responses).GetAccepted())
			// Closing input must not detach the stream from Runtime shutdown.
			stream.requests <- &agentv1.AccessRequest{Frame: &agentv1.AccessRequest_StdinEof{StdinEof: true}}
			switch action {
			case "stop":
				runtime.closeNetworkAccess()
			case "replace":
				runtime.setAccessState(&agentv1.AccessTarget{PrimaryContainerId: "primary", Version: 2}, nil)
			case "cancel":
				cancel()
			}
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(3 * time.Second):
				t.Fatal("TCP stream did not stop with its owner")
			}
			_, err = client.Read(make([]byte, 1))
			require.ErrorIs(t, err, io.EOF)
			next := &agentv1.AccessTarget{PrimaryContainerId: "next", Version: 3}
			runtime.setAccessState(next, nil)
			_, _, err = runtime.networkAccessContext(t.Context(), target)
			require.ErrorContains(t, err, "target changed")
			current, release, err := runtime.networkAccessContext(t.Context(), next)
			require.NoError(t, err)
			defer release()
			require.NoError(t, current.Err())
			t.Cleanup(runtime.closeNetworkAccess)
		})
	}
}
