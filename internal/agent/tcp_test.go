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

type fakeTCPAccessStream struct {
	requests  chan *agentv1.AccessRequest
	responses chan *agentv1.AccessResponse
}

func (s *fakeTCPAccessStream) Receive() (*agentv1.AccessRequest, error) {
	request, ok := <-s.requests
	if !ok {
		return nil, io.EOF
	}
	return request, nil
}

func (s *fakeTCPAccessStream) Send(response *agentv1.AccessResponse) error {
	s.responses <- response
	return nil
}

func tcpTestConnections(t *testing.T) (*net.TCPConn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	client, err := net.Dial("tcp4", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	server, err := listener.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	return client.(*net.TCPConn), server
}

func TestBridgeTCPConnectionPreservesTCPHalfClose(t *testing.T) {
	client, server := tcpTestConnections(t)

	stream := &fakeTCPAccessStream{requests: make(chan *agentv1.AccessRequest, 2), responses: make(chan *agentv1.AccessResponse, 8)}
	done := make(chan error, 1)
	go func() { done <- bridgeTCPConnection(context.Background(), stream, server) }()

	require.Equal(t, true, (<-stream.responses).GetAccepted())
	require.NoError(t, client.CloseWrite())
	require.Equal(t, true, (<-stream.responses).GetStdoutEof())
	select {
	case response := <-stream.responses:
		t.Fatalf("bridge finished before input half-close: %v", response.GetExitStatus())
	case <-time.After(100 * time.Millisecond):
	}

	stream.requests <- &agentv1.AccessRequest{Frame: &agentv1.AccessRequest_Stdin{Stdin: []byte("half-close")}}
	stream.requests <- &agentv1.AccessRequest{Frame: &agentv1.AccessRequest_StdinEof{StdinEof: true}}
	data, err := io.ReadAll(client)
	require.NoError(t, err)
	require.Equal(t, []byte("half-close"), data)
	require.Equal(t, int32(0), (<-stream.responses).GetExitStatus())
	require.NoError(t, <-done)
}

func TestBridgeTCPConnectionStopsOnContextCancellation(t *testing.T) {
	_, server := tcpTestConnections(t)

	stream := &fakeTCPAccessStream{requests: make(chan *agentv1.AccessRequest), responses: make(chan *agentv1.AccessResponse, 2)}
	defer close(stream.requests)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bridgeTCPConnection(ctx, stream, server) }()
	require.Equal(t, true, (<-stream.responses).GetAccepted())
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestBridgeTCPConnectionHandlesInputEOFBeforeOutputEOF(t *testing.T) {
	client, server := tcpTestConnections(t)

	stream := &fakeTCPAccessStream{requests: make(chan *agentv1.AccessRequest, 1), responses: make(chan *agentv1.AccessResponse, 8)}
	done := make(chan error, 1)
	go func() { done <- bridgeTCPConnection(context.Background(), stream, server) }()
	require.True(t, (<-stream.responses).GetAccepted())
	stream.requests <- &agentv1.AccessRequest{Frame: &agentv1.AccessRequest_StdinEof{StdinEof: true}}
	_, err := client.Write([]byte("output-after-input-eof"))
	require.NoError(t, err)
	require.NoError(t, client.CloseWrite())

	response := <-stream.responses
	require.Equal(t, []byte("output-after-input-eof"), response.GetStdout())
	require.True(t, (<-stream.responses).GetStdoutEof())
	require.Equal(t, int32(0), (<-stream.responses).GetExitStatus())
	require.NoError(t, <-done)
}
