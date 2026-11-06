// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConnectWaitsForLoopbackService(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := reserved.Addr().String()
	require.NoError(t, reserved.Close())
	_, portValue, err := net.SplitHostPort(address)
	require.NoError(t, err)
	port, err := strconv.ParseUint(portValue, 10, 16)
	require.NoError(t, err)
	serverDone := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		listener, err := net.Listen("tcp", address)
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = listener.Close() }()
		connection, err := listener.Accept()
		if err == nil {
			_, err = io.Copy(connection, bytes.NewBufferString("ready"))
			err = errors.Join(err, connection.Close())
		}
		serverDone <- err
	}()
	var output bytes.Buffer
	require.NoError(t, Connect(t.Context(), "127.0.0.1", uint16(port), 2*time.Second, bytes.NewReader(nil), &output))
	require.NoError(t, <-serverDone)
	require.Equal(t, "ready", output.String())
}
