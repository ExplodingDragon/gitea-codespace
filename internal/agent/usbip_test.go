// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"io"
	"net"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/stretchr/testify/require"
)

func TestUSBIPRuntimeCommands(t *testing.T) {
	runtime := &Runtime{USBIP: &USBIPRuntime{enabled: true, command: "/bin/echo"}}
	server := &RuntimeUSBIPServer{Runtime: runtime}
	sources := []*reverseListener{
		{id: "first", source: "workstation", port: 43111},
		{id: "second", source: "lab", port: 43112},
	}
	for _, source := range sources {
		require.NoError(t, runtime.forwards.register(source))
	}
	t.Cleanup(runtime.forwards.close)
	response, err := server.Run(t.Context(), &agentv1.RuntimeUSBIPServiceRunRequest{ProtocolVersion: 1, Operation: agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST})
	require.NoError(t, err)
	require.Len(t, response.Sources, 2)
	require.Equal(t, "lab", response.Sources[0].Name)
	require.Equal(t, "--tcp-port 43112 list --remote=127.0.0.1", response.Sources[0].Output)
	request := &agentv1.RuntimeUSBIPServiceRunRequest{ProtocolVersion: 1, Operation: agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_ATTACH, Argument: "2-1.3"}
	_, err = server.Run(t.Context(), request)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	request.Source = "workstation"
	response, err = server.Run(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "--tcp-port 43111 attach --remote=127.0.0.1 --busid=2-1.3", response.Output)
	request.Source = ""
	runtime.forwards.stop(sources[1], true)
	response, err = server.Run(t.Context(), request)
	require.NoError(t, err)
	require.Contains(t, response.Output, "--tcp-port 43111")
	for _, test := range []struct {
		operation      agentv1.RuntimeUSBIPOperation
		argument, want string
	}{
		{agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_STATUS, "", "port"},
		{agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_DETACH, "0", "detach --port=0"},
	} {
		response, err := server.Run(t.Context(), &agentv1.RuntimeUSBIPServiceRunRequest{ProtocolVersion: 1, Operation: test.operation, Argument: test.argument})
		require.NoError(t, err)
		require.Equal(t, test.want, response.Output)
	}
}

func TestReverseListenerCancellation(t *testing.T) {
	for _, source := range []string{"", "workstation"} {
		t.Run("source="+source, func(t *testing.T) {
			var registry reverseForwards
			listeners, err := listenTCPBind(t.Context(), "127.0.0.1", 0)
			require.NoError(t, err)
			state := &reverseListener{id: "first", source: source, listeners: listeners, port: uint32(listeners[0].Addr().(*net.TCPAddr).Port), pending: map[string]*pendingReverseConn{}, active: map[string]net.Conn{}}
			require.NoError(t, registry.register(state))
			t.Cleanup(registry.close)
			stream := &fakeTCPAccessStream{requests: make(chan *agentv1.AccessRequest, 1), responses: make(chan *agentv1.AccessResponse, 4)}
			done := make(chan error, 1)
			go func() { done <- registry.serve(t.Context(), state, stream) }()
			require.Equal(t, state.port, (<-stream.responses).GetReverseTcpReady().GetPort())
			client, err := net.DialTimeout("tcp4", listeners[0].Addr().String(), time.Second)
			require.NoError(t, err)
			defer client.Close()
			require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
			id := (<-stream.responses).GetReverseTcpConnection().GetConnectionId()
			connection, err := registry.accept(state.id, id)
			require.NoError(t, err)
			defer connection.Close()
			stream.requests <- &agentv1.AccessRequest{Frame: &agentv1.AccessRequest_StopListening{StopListening: true}}
			require.Equal(t, int32(0), (<-stream.responses).GetExitStatus())
			require.NoError(t, <-done)
			if source == "" {
				_, err = client.Write([]byte("alive"))
				require.NoError(t, err)
				buffer := make([]byte, 5)
				_, err = io.ReadFull(connection, buffer)
				require.NoError(t, err)
				require.Equal(t, "alive", string(buffer))
			} else {
				_, err = client.Read(make([]byte, 1))
				require.ErrorIs(t, err, io.EOF)
			}
			registry.close()
			require.Empty(t, registry.listeners)
			require.Zero(t, registry.connections)
		})
	}
}

func TestReverseSourceOwnership(t *testing.T) {
	var registry reverseForwards
	first := &reverseListener{id: "first", source: "lab", port: 42111}
	second := &reverseListener{id: "second", source: "lab", port: 42112}
	require.NoError(t, registry.register(first))
	require.Equal(t, connect.CodeAlreadyExists, connect.CodeOf(registry.register(second)))
	registry.stop(first, true)
	require.NoError(t, registry.register(second))
	registry.stop(first, true)
	require.Equal(t, uint32(42112), registry.sources()[0].Port)
	registry.close()
}
