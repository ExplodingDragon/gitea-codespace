// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protodelim"
)

func TestConnectHalfClose(t *testing.T) {
	for _, targetFirst := range []bool{false, true} {
		name := "input first"
		if targetFirst {
			name = "target first"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			require.NoError(t, err)
			defer func() { _ = listener.Close() }()
			control, helper := net.Pipe()
			defer func() { _ = control.Close() }()
			require.NoError(t, control.SetDeadline(time.Now().Add(3*time.Second)))
			done := make(chan error, 1)
			go func() { done <- Connect(ctx, "127.0.0.1", uint16(listener.Addr().(*net.TCPAddr).Port), helper, helper) }()
			target, err := listener.AcceptTCP()
			require.NoError(t, err)
			defer func() { _ = target.Close() }()
			require.NoError(t, target.SetDeadline(time.Now().Add(3*time.Second)))
			reader := bufio.NewReader(control)
			response := &agentv1.AccessResponse{}
			require.NoError(t, protodelim.UnmarshalFrom(reader, response))
			require.True(t, response.GetAccepted())
			sendInput := func() {
				_, err := protodelim.MarshalTo(control, &agentv1.AccessRequest{Frame: &agentv1.AccessRequest_Stdin{Stdin: []byte("request")}})
				require.NoError(t, err)
				_, err = protodelim.MarshalTo(control, &agentv1.AccessRequest{Frame: &agentv1.AccessRequest_StdinEof{StdinEof: true}})
				require.NoError(t, err)
				data, err := io.ReadAll(target)
				require.NoError(t, err)
				require.Equal(t, "request", string(data))
			}
			if !targetFirst {
				sendInput()
			}
			_, err = target.Write([]byte("response"))
			require.NoError(t, err)
			require.NoError(t, target.CloseWrite())
			require.NoError(t, protodelim.UnmarshalFrom(reader, response))
			require.Equal(t, "response", string(response.GetStdout()))
			require.NoError(t, protodelim.UnmarshalFrom(reader, response))
			require.True(t, response.GetStdoutEof())
			if targetFirst {
				sendInput()
			}
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("helper did not finish after both EOFs")
			}
		})
	}
}

func TestConnectCancellationAndDetach(t *testing.T) {
	for _, detach := range []bool{false, true} {
		name := "cancel"
		if detach {
			name = "detach after half-close"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			require.NoError(t, err)
			defer func() { _ = listener.Close() }()
			control, helper := net.Pipe()
			defer func() { _ = control.Close() }()
			require.NoError(t, control.SetDeadline(time.Now().Add(3*time.Second)))
			done := make(chan error, 1)
			go func() { done <- Connect(ctx, "127.0.0.1", uint16(listener.Addr().(*net.TCPAddr).Port), helper, helper) }()
			target, err := listener.AcceptTCP()
			require.NoError(t, err)
			defer func() { _ = target.Close() }()
			require.NoError(t, target.SetDeadline(time.Now().Add(3*time.Second)))
			require.NoError(t, protodelim.UnmarshalFrom(bufio.NewReader(control), &agentv1.AccessResponse{}))
			if detach {
				_, err = protodelim.MarshalTo(control, &agentv1.AccessRequest{Frame: &agentv1.AccessRequest_StdinEof{StdinEof: true}})
				require.NoError(t, err)
				require.NoError(t, control.Close())
			} else {
				cancel()
			}
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("helper remained blocked")
			}
			_, err = target.Read(make([]byte, 1))
			require.ErrorIs(t, err, io.EOF)
		})
	}
}
