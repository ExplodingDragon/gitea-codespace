// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
	"os"
	"testing"

	"connectrpc.com/connect"
	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	"gitea.dev/codespace-proto-go/agent/v1/agentv1connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type logRemote struct {
	agentv1connect.AgentControlServiceClient
	upload func(context.Context, *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error)
}

func (r *logRemote) UploadLogs(ctx context.Context, request *connect.Request[agentv1.UploadLogsRequest]) (*connect.Response[agentv1.UploadLogsResponse], error) {
	response, err := r.upload(ctx, request.Msg)
	return connect.NewResponse(response), err
}

func TestOperationLogReplayAndClosure(t *testing.T) {
	journal, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	log, err := OpenOperationLog(journal, 3, 120)
	require.NoError(t, err)
	require.NoError(t, log.Append([]*codespacev1.LogLine{{TimestampUnixNano: 1, Message: "building"}}))
	var first *agentv1.UploadLogsRequest
	remote := &logRemote{upload: func(_ context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		first = proto.Clone(request).(*agentv1.UploadLogsRequest)
		return nil, fmt.Errorf("response lost after append")
	}}
	require.ErrorContains(t, log.Flush(t.Context(), remote), "response lost")
	require.NoError(t, log.Close())
	// A reconnect may already know the remote end, but the pending batch must
	// still replay at its original byte offset after a process restart.
	log, err = OpenOperationLog(journal, 3, 155)
	require.NoError(t, err)
	remote.upload = func(_ context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		require.True(t, proto.Equal(first, request))
		return &agentv1.UploadLogsResponse{NextOffset: 155}, nil
	}
	require.NoError(t, log.Flush(t.Context(), remote))
	require.NoError(t, log.Append([]*codespacev1.LogLine{{TimestampUnixNano: 2, Message: "finished"}}))
	remote.upload = func(_ context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		require.EqualValues(t, 155, request.Offset)
		return &agentv1.UploadLogsResponse{NextOffset: request.Offset, Closed: true}, nil
	}
	require.NoError(t, log.Flush(t.Context(), remote))
	require.True(t, log.checkpoint.Closed)
	require.EqualValues(t, 155, log.checkpoint.Offset)
	require.NoError(t, log.Close())
	log, err = OpenOperationLog(journal, 3, 155)
	require.NoError(t, err)
	defer func() { require.NoError(t, log.Close()) }()
	require.True(t, log.checkpoint.Closed)
}

func TestOperationLogAppendDuringUpload(t *testing.T) {
	journal, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	defer func() { require.NoError(t, journal.Close()) }()
	log, err := OpenOperationLog(journal, 1, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, log.Close()) }()
	require.NoError(t, log.Append([]*codespacev1.LogLine{{TimestampUnixNano: 1, Message: "first"}}))
	requests := 0
	remote := &logRemote{upload: func(_ context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		requests++
		if requests == 1 {
			require.NoError(t, log.Append([]*codespacev1.LogLine{{TimestampUnixNano: 2, Message: "second"}}))
		} else {
			require.Equal(t, "second", request.Lines[0].Message)
		}
		return &agentv1.UploadLogsResponse{NextOffset: request.Offset + 25}, nil
	}}
	require.NoError(t, log.Flush(t.Context(), remote))
	require.Equal(t, 2, requests)
	require.EqualValues(t, 50, log.checkpoint.Offset)
}

func TestPruneRetiredOperationLogs(t *testing.T) {
	directory := t.TempDir()
	journal, err := OpenJournal(directory, VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	defer func() { require.NoError(t, journal.Close()) }()
	for _, version := range []int64{1, 2} {
		log, err := OpenOperationLog(journal, version, 0)
		require.NoError(t, err)
		require.NoError(t, log.Append([]*codespacev1.LogLine{{TimestampUnixNano: 1, Message: "operation output"}}))
		require.NoError(t, log.Close())
	}
	require.NoError(t, journal.PruneLogs(2))
	entries, err := os.ReadDir(directory + "/logs")
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{"2.json", "2.pb"}, names)
	log, err := OpenOperationLog(journal, 2, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, log.Close()) }()
	require.NoError(t, log.Flush(t.Context(), &logRemote{upload: func(_ context.Context, request *agentv1.UploadLogsRequest) (*agentv1.UploadLogsResponse, error) {
		require.Equal(t, "operation output", request.Lines[0].Message)
		return &agentv1.UploadLogsResponse{NextOffset: 100}, nil
	}}))
}
