// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logbuffer

import (
	"strings"
	"testing"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"github.com/stretchr/testify/require"
)

func TestBufferBoundsInflightAndReportsLoss(t *testing.T) {
	var buffer Buffer
	line := &codespacev1.LogLine{TimestampUnixNano: 1, Message: strings.Repeat("x", MaxLineBytes)}
	added := 0
	for range 100 {
		added += buffer.Add([]*codespacev1.LogLine{line}, MaxBytes)
	}
	require.Equal(t, added, buffer.Bytes())
	require.LessOrEqual(t, buffer.Bytes(), MaxBytes)
	before := buffer.Bytes()
	lines, size := buffer.Take()
	require.NotEmpty(t, lines)
	require.LessOrEqual(t, size, MaxBatchBytes)
	require.Equal(t, before, buffer.Bytes())
	require.Zero(t, buffer.Add([]*codespacev1.LogLine{line}, 0))
	require.Equal(t, before, buffer.Bytes())
	buffer.Release(size)
	added = buffer.Add(nil, MaxBytes)
	require.Positive(t, added)
	require.Equal(t, before-size+added, buffer.Bytes())
	var messages []string
	for buffer.Pending() {
		lines, size := buffer.Take()
		for _, line := range lines {
			messages = append(messages, line.Message)
		}
		buffer.Release(size)
	}
	require.Contains(t, messages[len(messages)-1], "lines lost")
	require.Zero(t, buffer.Bytes())
}
