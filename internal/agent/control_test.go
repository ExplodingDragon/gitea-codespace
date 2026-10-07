// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	"gitea.dev/codespace-proto-go/agent/v1/agentv1connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

type controlService struct {
	agentv1connect.UnimplementedAgentControlServiceHandler
	control func(context.Context, *connect.BidiStream[agentv1.ControlRequest, agentv1.ControlResponse]) error
	upload  func(context.Context, *connect.Request[agentv1.UploadLogsRequest]) (*connect.Response[agentv1.UploadLogsResponse], error)
}

func (s controlService) Control(ctx context.Context, stream *connect.BidiStream[agentv1.ControlRequest, agentv1.ControlResponse]) error {
	return s.control(ctx, stream)
}

func (s controlService) UploadLogs(ctx context.Context, request *connect.Request[agentv1.UploadLogsRequest]) (*connect.Response[agentv1.UploadLogsResponse], error) {
	if s.upload == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("log upload is not configured by this test"))
	}
	return s.upload(ctx, request)
}

func TestControlClientPersistsResultAcrossLostAcknowledgement(t *testing.T) {
	journal, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	defer func() { require.NoError(t, journal.Close()) }()
	var executions, results, payloads atomic.Int32
	resultReceived := make(chan struct{}, 8)
	uploaded := make(chan *agentv1.UploadLogsRequest, 8)
	operation := &codespacev1.OperationPayload{RuntimeUuid: "runtime", OperationRversion: 1, LogOffset: 120, Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{}}}
	service := controlService{control: func(ctx context.Context, stream *connect.BidiStream[agentv1.ControlRequest, agentv1.ControlResponse]) error {
		for {
			request, err := stream.Receive()
			if err != nil {
				return err
			}
			response := &agentv1.ControlResponse{SessionId: request.SessionId, Sequence: request.Sequence, OperationRversion: 1}
			if request.AcceptedOperationRversion != 1 {
				response.Operation = operation
				response.Runtime = &agentv1.RuntimeOptions{GitSshKeyType: "ed25519"}
				payloads.Add(1)
			}
			if request.Report.Result != nil {
				results.Add(1)
				resultReceived <- struct{}{}
				// A real HTTP/2 disconnect after recording the result loses its ack.
				return connect.NewError(connect.CodeUnavailable, fmt.Errorf("reply lost"))
			}
			if len(request.Report.GitSshPublicKey) == 0 {
				response.CancelExecution = true
			} else {
				if _, err := ssh.ParsePublicKey(request.Report.GitSshPublicKey); err != nil {
					return err
				}
				response.Access = &codespacev1.RuntimeAccessBundle{GiteaToken: "test-token"}
				response.PermitValidForMilliseconds = 2000
			}
			if err := stream.Send(response); err != nil {
				return err
			}
		}
	}}
	service.upload = func(_ context.Context, request *connect.Request[agentv1.UploadLogsRequest]) (*connect.Response[agentv1.UploadLogsResponse], error) {
		uploaded <- request.Msg
		return connect.NewResponse(&agentv1.UploadLogsResponse{NextOffset: request.Msg.Offset + 100}), nil
	}
	_, handler := agentv1connect.NewAgentControlServiceHandler(service)
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	remote := agentv1connect.NewAgentControlServiceClient(server.Client(), server.URL, connect.WithGRPC())
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		client := &ControlClient{Journal: journal, Remote: remote, Execute: func(_ context.Context, response *agentv1.ControlResponse, report func(*agentv1.AgentReport), stdout, stderr io.Writer) error {
			executions.Add(1)
			_, _ = io.WriteString(stdout, "build test-")
			_, _ = io.WriteString(stdout, "token\n")
			report(&agentv1.AgentReport{OperationRversion: 1, Boot: &codespacev1.RuntimeBoot{OperationRversion: 1}, Target: &agentv1.AccessTarget{Version: 1}})
			return nil
		}}
		done := make(chan error, 1)
		go func() { done <- client.Run(ctx) }()
		select {
		case <-resultReceived:
		case <-ctx.Done():
			t.Fatal("Agent did not report its result")
		}
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	}
	require.EqualValues(t, 1, executions.Load())
	require.GreaterOrEqual(t, results.Load(), int32(2))
	require.EqualValues(t, 2, payloads.Load())
	select {
	case batch := <-uploaded:
		require.EqualValues(t, 120, batch.Offset)
		require.Len(t, batch.Lines, 1)
		require.Equal(t, "build [redacted]", batch.Lines[0].Message)
	default:
		t.Fatal("Agent result was reported without uploading its available output")
	}
}

func TestControlClientCancelsExpiredPermitDuringStalledResponse(t *testing.T) {
	journal, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	defer func() { require.NoError(t, journal.Close()) }()
	started := make(chan struct{})
	canceled := make(chan struct{})
	service := controlService{control: func(ctx context.Context, stream *connect.BidiStream[agentv1.ControlRequest, agentv1.ControlResponse]) error {
		for {
			request, err := stream.Receive()
			if err != nil {
				return err
			}
			select {
			case <-started:
				<-ctx.Done()
				return ctx.Err()
			default:
			}
			response := &agentv1.ControlResponse{SessionId: request.SessionId, Sequence: request.Sequence, OperationRversion: 1,
				Operation: &codespacev1.OperationPayload{RuntimeUuid: "runtime", OperationRversion: 1, Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{}}},
				Runtime:   &agentv1.RuntimeOptions{GitSshKeyType: "ed25519"}, CancelExecution: len(request.Report.GitSshPublicKey) == 0,
				Access: &codespacev1.RuntimeAccessBundle{}, PermitValidForMilliseconds: 500,
			}
			if err := stream.Send(response); err != nil {
				return err
			}
		}
	}}
	_, handler := agentv1connect.NewAgentControlServiceHandler(service)
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := &ControlClient{Journal: journal, Remote: agentv1connect.NewAgentControlServiceClient(server.Client(), server.URL, connect.WithGRPC()), Execute: func(ctx context.Context, _ *agentv1.ControlResponse, _ func(*agentv1.AgentReport), stdout, stderr io.Writer) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("execution did not start")
	}
	expiryDeadline := time.NewTimer(2 * time.Second)
	defer expiryDeadline.Stop()
	select {
	case <-canceled:
	case <-expiryDeadline.C:
		t.Fatal("execution survived an expired permit")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestControlClientDrainsPriorExecutionBeforeNewOperation(t *testing.T) {
	journal, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	defer func() { require.NoError(t, journal.Close()) }()
	var version atomic.Int64
	version.Store(1)
	var running atomic.Int32
	var overlap atomic.Bool
	firstCanceled := make(chan struct{})
	secondFinished := make(chan struct{})
	service := controlService{control: func(ctx context.Context, stream *connect.BidiStream[agentv1.ControlRequest, agentv1.ControlResponse]) error {
		for {
			request, err := stream.Receive()
			if err != nil {
				return err
			}
			operation := &codespacev1.OperationPayload{RuntimeUuid: "runtime", OperationRversion: version.Load(), Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{}}}
			if operation.OperationRversion == 2 {
				operation.Command = &codespacev1.OperationPayload_Resume{Resume: &codespacev1.ResumeOperationPayload{}}
			}
			response := &agentv1.ControlResponse{SessionId: request.SessionId, Sequence: request.Sequence, OperationRversion: operation.OperationRversion, Operation: operation, Runtime: &agentv1.RuntimeOptions{GitSshKeyType: "ed25519"},
				CancelExecution: len(request.Report.GitSshPublicKey) == 0, PermitValidForMilliseconds: 2000, Access: &codespacev1.RuntimeAccessBundle{}}
			if err := stream.Send(response); err != nil {
				return err
			}
		}
	}}
	_, handler := agentv1connect.NewAgentControlServiceHandler(service)
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	client := &ControlClient{Journal: journal, Remote: agentv1connect.NewAgentControlServiceClient(server.Client(), server.URL, connect.WithGRPC()), Execute: func(ctx context.Context, response *agentv1.ControlResponse, _ func(*agentv1.AgentReport), stdout, stderr io.Writer) error {
		if running.Add(1) != 1 {
			overlap.Store(true)
		}
		defer running.Add(-1)
		if response.Operation.OperationRversion == 1 {
			version.Store(2)
			<-ctx.Done()
			close(firstCanceled)
			return ctx.Err()
		}
		close(secondFinished)
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-secondFinished:
	case <-ctx.Done():
		t.Fatal("new operation was not executed")
	}
	select {
	case <-firstCanceled:
	default:
		t.Fatal("new operation started before cancellation")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.False(t, overlap.Load())
	data, err := journal.read("state/report.pb", 1024*1024)
	require.NoError(t, err)
	report := &agentv1.AgentReport{}
	require.NoError(t, proto.Unmarshal(data, report))
	require.NotNil(t, report.Result)
	require.EqualValues(t, 2, report.Result.OperationRversion)
}
