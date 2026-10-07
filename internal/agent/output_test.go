// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"

	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	"github.com/stretchr/testify/require"
)

func TestOutputRedactsBeforePersistingAndPreservesStreams(t *testing.T) {
	directory := t.TempDir()
	journal, err := OpenJournal(directory, VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	defer func() { require.NoError(t, journal.Close()) }()
	log, err := OpenOperationLog(journal, 1, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, log.Close()) }()
	output := NewOutput(log, []string{"secret-token", "multi\nline-secret"})
	_, err = fmt.Fprint(output.Stdout(), "prefix secret-")
	require.NoError(t, err)
	_, err = fmt.Fprint(output.Stderr(), "separate error\n")
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stdout(), "token suffix")
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stdout(), base64.StdEncoding.EncodeToString([]byte("secret-token")))
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stderr(), "Authorization: Bearer private-token https://user:pass@registry.example/path")
	require.NoError(t, err)
	_, err = fmt.Fprint(output.Stdout(), "line-secret")
	require.NoError(t, err)
	output.FlushLines()
	require.NoError(t, output.Err())
	var messages []string
	remote := &logRemote{upload: func(_ context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		for _, line := range request.Lines {
			messages = append(messages, line.Message)
		}
		return &agentv1.UploadLogsResponse{NextOffset: request.Offset + 100}, nil
	}}
	require.NoError(t, log.Flush(t.Context(), remote))
	require.Equal(t, []string{"separate error", "prefix [redacted] suffix", "[redacted]", "Authorization: Bearer [redacted] https://[redacted]@registry.example/path", "[redacted]"}, messages)
	spool, err := os.ReadFile(directory + "/logs/1.pb")
	require.NoError(t, err)
	// Check the actual stored spool, rather than only its rendered messages.
	require.NotContains(t, string(spool), "secret-token")
	require.NotContains(t, string(spool), "line-secret")
}

func TestOutputBoundsLongLinesAndSpool(t *testing.T) {
	journal, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	defer func() { require.NoError(t, journal.Close()) }()
	log, err := OpenOperationLog(journal, 1, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, log.Close()) }()
	output := NewOutput(log, []string{"secret-split-across-boundary"})
	_, err = fmt.Fprint(output.Stdout(), strings.Repeat("x", maxOutputLineBytes-7)+"secret-")
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stdout(), "split-across-boundary")
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stdout(), "next line")
	require.NoError(t, err)
	output.FlushLines()
	var messages []string
	require.NoError(t, log.Flush(t.Context(), &logRemote{upload: func(_ context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		for _, line := range request.Lines {
			messages = append(messages, line.Message)
		}
		return &agentv1.UploadLogsResponse{NextOffset: request.Offset + 100}, nil
	}}))
	require.Equal(t, []string{"[output line omitted: exceeds 32 KiB]", "next line"}, messages)
	// A sparse file exercises the capacity boundary without generating 32 MiB
	// of test logs or changing production limits for a test.
	require.NoError(t, log.file.Truncate(maxLogFileBytes-1100))
	log.size = maxLogFileBytes - 1100
	_, err = fmt.Fprintln(output.Stdout(), strings.Repeat("y", 1000))
	require.NoError(t, err)
	output.FlushLines()
	require.True(t, output.truncated)
	before := log.size
	_, err = fmt.Fprintln(output.Stdout(), "further output")
	require.NoError(t, err)
	output.FlushLines()
	require.Equal(t, before, log.size)
	require.LessOrEqual(t, log.size, int64(maxLogFileBytes))
	require.NoError(t, output.Err())
}
