// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"
	"time"

	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	"github.com/stretchr/testify/require"
)

func TestExecutionPermitTimingAndReconnect(t *testing.T) {
	var permit Permit
	permit.Reconnect()
	now := time.Now()
	session, sequence, err := permit.Request(7, now)
	require.NoError(t, err)
	response := &agentv1.ControlResponse{SessionId: session, Sequence: sequence, PermitValidForMilliseconds: 1000, OperationRversion: 7}
	deadline, err := permit.Accept(response, now.Add(900*time.Millisecond))
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Second), deadline)
	require.True(t, permit.Valid(7, now.Add(999*time.Millisecond)))
	require.False(t, permit.Valid(7, now.Add(time.Second)))
	_, err = permit.Accept(response, now.Add(2*time.Second))
	require.Error(t, err)
	_, _, err = permit.Request(7, now.Add(2*time.Second))
	require.NoError(t, err)
	_, err = permit.Accept(response, now.Add(2*time.Second))
	require.Error(t, err)
	permit.Reconnect()
	session, sequence, err = permit.Request(8, now.Add(3*time.Second))
	require.NoError(t, err)
	_, err = permit.Accept(response, now.Add(3*time.Second))
	require.Error(t, err)
	response.SessionId, response.Sequence, response.OperationRversion = session, sequence, 8
	_, err = permit.Accept(response, now.Add(3*time.Second))
	require.NoError(t, err)
	response.CancelExecution = true
	_, err = permit.Accept(response, now.Add(3*time.Second))
	require.Error(t, err)
	require.False(t, permit.Valid(8, now.Add(3*time.Second)))
}
