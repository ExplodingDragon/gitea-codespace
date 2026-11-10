// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"bytes"
	"testing"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protodelim"
)

func TestTCPAccessOutput(t *testing.T) {
	var dockerOutput bytes.Buffer
	stdout := stdcopy.NewStdWriter(&dockerOutput, stdcopy.Stdout)
	stderr := stdcopy.NewStdWriter(&dockerOutput, stdcopy.Stderr)
	_, err := stderr.Write([]byte("diagnostic before acceptance"))
	require.NoError(t, err)
	want := []*agentv1.AccessResponse{
		{Frame: &agentv1.AccessResponse_Accepted{Accepted: true}},
		{Frame: &agentv1.AccessResponse_Stdout{Stdout: []byte("payload")}},
		{Frame: &agentv1.AccessResponse_StdoutEof{StdoutEof: true}},
	}
	for _, frame := range want {
		var encoded bytes.Buffer
		_, err := protodelim.MarshalTo(&encoded, frame)
		require.NoError(t, err)
		// Docker frames can split a protobuf message at any byte.
		for _, b := range encoded.Bytes() {
			_, err = stdout.Write([]byte{b})
			require.NoError(t, err)
		}
		_, err = stderr.Write([]byte("diagnostic"))
		require.NoError(t, err)
	}
	var received []*agentv1.AccessResponse
	require.NoError(t, sendTCPAccessOutput(func(frame *agentv1.AccessResponse) error {
		received = append(received, frame)
		return nil
	}, &dockerOutput))
	require.Len(t, received, 3)
	require.True(t, received[0].GetAccepted())
	require.Equal(t, "payload", string(received[1].GetStdout()))
	require.True(t, received[2].GetStdoutEof())

	dockerOutput.Reset()
	_, err = stderr.Write([]byte("connect: connection refused"))
	require.NoError(t, err)
	err = sendTCPAccessOutput(func(*agentv1.AccessResponse) error {
		t.Fatal("failed connection was accepted")
		return nil
	}, &dockerOutput)
	require.ErrorContains(t, err, "connection refused")
}
