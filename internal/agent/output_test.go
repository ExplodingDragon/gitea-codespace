// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/logbuffer"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"gitea.dev/codespace/internal/runtimeevent"
	"github.com/stretchr/testify/require"
)

type logRemote struct {
	agentv1connect.AgentControlServiceClient
	upload func(context.Context, *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error)
	event  func(context.Context, *agentv1.ReportEventRequest) (*agentv1.ReportEventResponse, error)
}

func (r *logRemote) ReportEvent(ctx context.Context, request *agentv1.ReportEventRequest) (*agentv1.ReportEventResponse, error) {
	return r.event(ctx, request)
}

func (r *logRemote) UploadLogs(ctx context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
	return r.upload(ctx, request)
}

func TestRuntimeEventsRedactAndForward(t *testing.T) {
	response := &agentv1.ControlResponse{
		Access: &codespacev1.RuntimeAccessBundle{GiteaToken: "private-token"},
		Runtime: &agentv1.RuntimeOptions{Cache: &agentv1.RuntimeCache{
			Credentials: map[string]*agentv1.RegistryCredential{"registry.example": {Password: "registry-password"}},
		}},
	}
	runtime := &Runtime{eventRedactor: logbuffer.NewRedactor(logCredentials(response))}
	var received []*agentv1.ReportEventRequest
	remote := &logRemote{event: func(ctx context.Context, request *agentv1.ReportEventRequest) (*agentv1.ReportEventResponse, error) {
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline)
		received = append(received, request)
		if request.Type == "limited" {
			return nil, connect.NewError(connect.CodeResourceExhausted, "event rate exceeded")
		}
		return &agentv1.ReportEventResponse{}, nil
	}}
	server := &RuntimeEventServer{Runtime: runtime, Remote: remote}
	_, err := server.Emit(t.Context(), &agentv1.EmitRequest{Type: "private-token", Message: "first private-token\nsecond " + base64.StdEncoding.EncodeToString([]byte("private-token")) + "\ncache registry-password"})
	require.NoError(t, err)
	_, err = server.Emit(t.Context(), &agentv1.EmitRequest{Type: strings.Repeat("a", runtimeevent.MaxTypeBytes), Message: strings.Repeat("x", runtimeevent.MaxMessageBytes)})
	require.NoError(t, err)
	_, err = server.Emit(t.Context(), &agentv1.EmitRequest{Type: "limited", Message: "message"})
	require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
	for _, request := range []*agentv1.EmitRequest{
		{Type: "", Message: "message"}, {Type: "bad\ntype", Message: "message"},
		{Type: strings.Repeat("a", runtimeevent.MaxTypeBytes+1), Message: "message"},
		{Type: "test", Message: ""}, {Type: "test", Message: strings.Repeat("x", runtimeevent.MaxMessageBytes+1)},
		{Type: "test", Message: string([]byte{0xff})},
	} {
		_, err := server.Emit(t.Context(), request)
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	}
	require.Len(t, received, 3)
	require.Equal(t, "redacted", received[0].Type)
	require.Equal(t, "first [redacted]\nsecond [redacted]\ncache [redacted]", received[0].Message)
	runtime.eventRedactor = logbuffer.NewRedactor([]string{"pin"})
	_, err = server.Emit(t.Context(), &agentv1.EmitRequest{Type: "budget", Message: strings.Repeat("pin", runtimeevent.MaxMessageBytes/3)})
	require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
	require.Len(t, received, 3)
}

func TestOutputRedactsStreamsAndBoundsLines(t *testing.T) {
	output := NewOutput(1, []string{"secret-token", "multi\nline-secret", "secret-split-across-boundary"})
	_, err := fmt.Fprint(output.Stdout(), "prefix secret-")
	require.NoError(t, err)
	_, err = fmt.Fprint(output.Stderr(), "separate error\n")
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stdout(), "to\rken suffix\r")
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stdout(), base64.StdEncoding.EncodeToString([]byte("secret-token")))
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stdout(), base64.StdEncoding.EncodeToString([]byte("multi\nline-secret")))
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stderr(), "Authorization: Bearer private-token https://user:pass@registry.example/path")
	require.NoError(t, err)
	_, err = fmt.Fprint(output.Stdout(), strings.Repeat("x", logbuffer.MaxLineBytes-7)+"secret-")
	require.NoError(t, err)
	_, err = fmt.Fprintln(output.Stdout(), "split-across-boundary")
	require.NoError(t, err)
	_, err = fmt.Fprint(output.Stdout(), "line-secret")
	require.NoError(t, err)
	output.FlushLines()
	var messages []string
	output.Upload(t.Context(), &logRemote{upload: func(_ context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		for _, line := range request.Lines {
			messages = append(messages, line.Message)
		}
		return &agentv1.UploadLogsResponse{}, nil
	}})
	require.Equal(t, []string{"separate error", "prefix [redacted] suffix", "[redacted]", "[redacted]", "Authorization: Bearer [redacted] https://[redacted]@registry.example/path", "[WARN] [logging] Output line omitted: exceeds 32 KiB", "[redacted]"}, messages)
}

func TestOutputUploadsWhileLowRateOutputContinues(t *testing.T) {
	output := NewOutput(1, nil)
	uploaded := make(chan struct{}, 1)
	remote := &logRemote{upload: func(_ context.Context, _ *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		select {
		case uploaded <- struct{}{}:
		default:
		}
		return &agentv1.UploadLogsResponse{}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); output.Upload(ctx, remote) }()
	defer func() { cancel(); <-done }()
	for range 15 {
		_, err := fmt.Fprintln(output.Stdout(), "still producing output")
		require.NoError(t, err)
		select {
		case <-uploaded:
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
	t.Fatal("continuous output postponed the first upload")
}

func TestOutputContinuesAfterUncertainUpload(t *testing.T) {
	output := NewOutput(1, nil)
	_, err := fmt.Fprintln(output.Stdout(), "first batch")
	require.NoError(t, err)
	var messages []string
	calls := 0
	remote := &logRemote{upload: func(_ context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		calls++
		for _, line := range request.Lines {
			messages = append(messages, line.Message)
		}
		if calls == 1 {
			return nil, errors.New("reply lost")
		}
		return &agentv1.UploadLogsResponse{}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); output.Upload(ctx, remote) }()
	defer func() { cancel(); <-done }()
	require.Eventually(t, func() bool {
		output.mu.Lock()
		defer output.mu.Unlock()
		return output.buffer.Bytes() == 0
	}, 3*time.Second, 10*time.Millisecond)
	_, err = fmt.Fprintln(output.Stdout(), "later batch")
	require.NoError(t, err)
	output.FlushLines()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("output did not drain")
	}
	require.Equal(t, []string{"first batch", "[WARN] [logging] Operation output omitted: 1 lines lost", "later batch"}, messages)
}
