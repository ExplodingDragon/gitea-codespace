// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/logbuffer"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type operationLogRemote struct {
	codespacev1connect.ManagerServiceClient
	update func(context.Context, *codespacev1.UpdateLogRequest) (*codespacev1.UpdateLogResponse, error)
}

func (r *operationLogRemote) UpdateLog(ctx context.Context, request *codespacev1.UpdateLogRequest) (*codespacev1.UpdateLogResponse, error) {
	return r.update(ctx, request)
}

func testOperationLogs(t *testing.T, remote *operationLogRemote) (*codespaceLogs, *api.Codespace) {
	t.Helper()
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{UID: "runtime-uid", Namespace: "site"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{Name: "site", UID: "site-uid"}, CodespaceID: 1, Operation: api.Operation{Type: "create", Version: 2}}, Status: api.CodespaceStatus{}}
	logs := &codespaceLogs{eventMaxPerSecond: 5, remote: func(context.Context, api.ResourceReference) (codespacev1connect.ManagerServiceClient, error) {
		return remote, nil
	}}
	logs.OpenSite(t.Context(), cs.Spec.Site.UID)
	t.Cleanup(func() { logs.CloseSite(cs.Spec.Site.UID) })
	return logs, cs
}

func TestCodespaceEventsContinueAfterCompletionAndElection(t *testing.T) {
	var mu sync.Mutex
	var messages []string
	offset := int64(100)
	logs, cs := testOperationLogs(t, &operationLogRemote{update: func(_ context.Context, request *codespacev1.UpdateLogRequest) (*codespacev1.UpdateLogResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		for _, line := range request.Lines {
			messages = append(messages, line.Message)
			offset += int64(len(line.Message) + 1)
		}
		return &codespacev1.UpdateLogResponse{NextOffset: offset}, nil
	}})
	require.NoError(t, logs.Append(cs, "", nil))
	cs.Status.Result = &api.OperationResult{Version: cs.Spec.Operation.Version, Succeeded: true}
	cs.Status.Target = &api.AgentTarget{Ready: true}
	cs.Status.Pod.UID = "pod-uid"
	logs.CompleteOperation(cs)
	logs.CompleteOperation(cs)
	cs.Status.SettledOperationVersion = cs.Spec.Operation.Version
	require.NoError(t, logs.ReportEvent(cs, "webide.error", "first\nsecond"))
	awaitOperationLogDelivery(t, logs, cs)
	logs.mu.Lock()
	require.True(t, logs.senders[cs.UID].drainDeadline.IsZero())
	logs.mu.Unlock()
	logs.CloseSite(cs.Spec.Site.UID)
	logs.OpenSite(t.Context(), cs.Spec.Site.UID)
	require.NoError(t, logs.ReportEvent(cs, "workspace.notice", "after election"))
	awaitOperationLogDelivery(t, logs, cs)
	logs.CloseSite(cs.Spec.Site.UID)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{
		"[INFO] [operation] Accepted create #2", "[INFO] [operation] create #2 completed",
		"[EVENT] [webide.error] first", "[EVENT] [webide.error] second",
		"[EVENT] [workspace.notice] after election",
	}, messages)
}

func TestCodespaceEventsRateAndQueueAdmission(t *testing.T) {
	logs, cs := testOperationLogs(t, &operationLogRemote{update: func(ctx context.Context, _ *codespacev1.UpdateLogRequest) (*codespacev1.UpdateLogResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	var workers sync.WaitGroup
	errors := make(chan error, 20)
	for range cap(errors) {
		workers.Go(func() { errors <- logs.ReportEvent(cs, "parallel", "message") })
	}
	workers.Wait()
	close(errors)
	accepted := 0
	for err := range errors {
		if err == nil {
			accepted++
		} else {
			require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
		}
	}
	require.Equal(t, 5, accepted)
	other := cs.DeepCopy()
	other.UID = "other-runtime"
	require.NoError(t, logs.ReportEvent(other, "parallel", "message"))
	cs.Spec.Operation.Version++
	cs.Spec.Operation.Type = "stop"
	logs.RetainSite(cs.Spec.Site.UID, []api.Codespace{*cs, *other})
	require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(logs.ReportEvent(cs, "different-type", "message")))
	logs.mu.Lock()
	logs.eventMaxPerSecond = 2
	sender := logs.senders[cs.UID]
	sender.eventTimes = []time.Time{time.Now().Add(-2 * time.Second), time.Now()}
	logs.mu.Unlock()
	require.NoError(t, logs.ReportEvent(cs, "window", "message"))
	require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(logs.ReportEvent(cs, "window", "message")))
	logs.mu.Lock()
	sender.eventTimes = nil
	before := sender.buffer.Bytes()
	logs.mu.Unlock()
	// A legal multiline input can exceed the queue budget after per-line framing.
	require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(logs.ReportEvent(cs, "large", strings.Repeat("\n", 16384))))
	logs.mu.Lock()
	require.Equal(t, before, sender.buffer.Bytes())
	require.Empty(t, sender.eventTimes)
	logs.mu.Unlock()
	cs.Status.Result = &api.OperationResult{Version: cs.Spec.Operation.Version, Succeeded: true}
	logs.CompleteOperation(cs)
	logs.mu.Lock()
	require.False(t, sender.drainDeadline.IsZero())
	sender.drainDeadline = time.Now().Add(-time.Second)
	logs.mu.Unlock()
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(logs.ReportEvent(cs, "stopped", "message")))
}

func awaitOperationLogDelivery(t *testing.T, logs *codespaceLogs, cs *api.Codespace) {
	t.Helper()
	require.Eventually(t, func() bool {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		return logs.senders[cs.UID].buffer.Bytes() == 0
	}, 4*time.Second, time.Millisecond)
}

func TestOperationLogsReplayAndRestart(t *testing.T) {
	var mu sync.Mutex
	offset := int64(10)
	var requests []*codespacev1.UpdateLogRequest
	var messages []string
	var lost *codespacev1.UpdateLogRequest
	var starts []int64
	remote := &operationLogRemote{update: func(_ context.Context, request *codespacev1.UpdateLogRequest) (*codespacev1.UpdateLogResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(request.Lines) == 0 {
			starts = append(starts, offset)
			return &codespacev1.UpdateLogResponse{NextOffset: offset}, nil
		}
		requests = append(requests, proto.Clone(request).(*codespacev1.UpdateLogRequest))
		if request.Offset < offset {
			if !proto.Equal(request, lost) {
				return nil, connect.NewError(connect.CodeInvalidArgument, "changed replay")
			}
			return &codespacev1.UpdateLogResponse{NextOffset: offset}, nil
		}
		for _, line := range request.Lines {
			offset += int64(len(line.Message) + 1)
			messages = append(messages, line.Message)
		}
		if lost == nil {
			lost = proto.Clone(request).(*codespacev1.UpdateLogRequest)
			return nil, connect.NewError(connect.CodeUnavailable, "response lost after append")
		}
		return &codespacev1.UpdateLogResponse{NextOffset: offset}, nil
	}}
	logs, cs := testOperationLogs(t, remote)
	require.NoError(t, logs.Append(cs, "", nil))
	require.NoError(t, logs.Append(cs, "pod", []*codespacev1.LogLine{{TimestampUnixNano: 1, Message: "Creating Pod"}}))
	require.NoError(t, logs.Append(cs, diagnosticLogStage, []*codespacev1.LogLine{{TimestampUnixNano: 1, Message: "Pod snapshot"}}))
	require.NoError(t, logs.Append(cs, "pod", []*codespacev1.LogLine{{TimestampUnixNano: 1, Message: "Creating Pod"}}))
	require.NoError(t, logs.Append(cs, "", []*codespacev1.LogLine{{TimestampUnixNano: 2, Message: "Agent started"}}))
	cs.Status.Result = &api.OperationResult{Version: 2, Succeeded: true}
	logs.CompleteOperation(cs)
	awaitOperationLogDelivery(t, logs, cs)
	logs.CloseSite(cs.Spec.Site.UID)
	mu.Lock()
	require.Equal(t, []string{"[INFO] [operation] Accepted create #2", "Creating Pod", "Pod snapshot", "Agent started", "[INFO] [operation] create #2 completed"}, messages)
	require.True(t, proto.Equal(requests[0], requests[1]))
	restartOffset := offset
	mu.Unlock()
	logs.OpenSite(t.Context(), cs.Spec.Site.UID)
	require.NoError(t, logs.Append(cs, "", []*codespacev1.LogLine{{TimestampUnixNano: 3, Message: "After restart"}}))
	cs.Status.Result.Succeeded = false
	logs.CompleteOperation(cs)
	awaitOperationLogDelivery(t, logs, cs)
	logs.CloseSite(cs.Spec.Site.UID)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int64{10, restartOffset}, starts)
	require.Equal(t, "After restart", messages[len(messages)-2])
	require.Equal(t, "[ERROR] [operation] create #2 failed; see preceding output", messages[len(messages)-1])
}

func TestOperationLogsResynchronizeConflictingOffset(t *testing.T) {
	conflict := make(chan struct{})
	release := make(chan struct{})
	queries := 0
	var received []string
	detail, err := connectproto.NewErrorDetail(&codespacev1.FailureDetail{Category: "offset_conflict"})
	require.NoError(t, err)
	remote := &operationLogRemote{update: func(ctx context.Context, request *codespacev1.UpdateLogRequest) (*codespacev1.UpdateLogResponse, error) {
		if len(request.Lines) == 0 {
			queries++
			return &codespacev1.UpdateLogResponse{NextOffset: 50}, nil
		}
		if queries == 1 {
			close(conflict)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, connect.NewError(connect.CodeAborted, "offset conflict").WithDetail(detail)
		}
		for _, line := range request.Lines {
			received = append(received, line.Message)
		}
		return &codespacev1.UpdateLogResponse{NextOffset: request.Offset + 100}, nil
	}}
	logs, cs := testOperationLogs(t, remote)
	require.NoError(t, logs.Append(cs, "", nil))
	select {
	case <-conflict:
	case <-time.After(time.Second):
		t.Fatal("sender did not start")
	}
	require.NoError(t, logs.Append(cs, "", []*codespacev1.LogLine{{TimestampUnixNano: 1, Message: "later output"}}))
	close(release)
	awaitOperationLogDelivery(t, logs, cs)
	logs.CloseSite(cs.Spec.Site.UID)
	require.Equal(t, 2, queries)
	require.Equal(t, []string{"later output", "[WARN] [logging] Operation output omitted: 1 lines lost"}, received)
}

func TestOperationLogsCloseOnPermanentFailure(t *testing.T) {
	for _, category := range []string{"stale_operation", "codespace_not_found", "log_size_exceeded", "invalid_input"} {
		t.Run(category, func(t *testing.T) {
			detail, err := connectproto.NewErrorDetail(&codespacev1.FailureDetail{Category: category})
			require.NoError(t, err)
			logs, cs := testOperationLogs(t, &operationLogRemote{update: func(context.Context, *codespacev1.UpdateLogRequest) (*codespacev1.UpdateLogResponse, error) {
				return nil, connect.NewError(connect.CodeInvalidArgument, "permanent failure").WithDetail(detail)
			}})
			require.NoError(t, logs.Append(cs, "", nil))
			logs.mu.Lock()
			done := logs.senders[cs.UID].done
			logs.mu.Unlock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("sender did not close")
			}
			logs.CompleteOperation(cs)
			require.ErrorIs(t, logs.Append(cs, "", nil), errCodespaceLogClosed)
		})
	}
}

func TestOperationLogsBoundMemoryAndFinalDrain(t *testing.T) {
	remote := &operationLogRemote{update: func(ctx context.Context, _ *codespacev1.UpdateLogRequest) (*codespacev1.UpdateLogResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	logs, cs := testOperationLogs(t, remote)
	line := &codespacev1.LogLine{TimestampUnixNano: 1, Message: strings.Repeat("x", logbuffer.MaxLineBytes)}
	for i := range 70 {
		cs.UID = types.UID(fmt.Sprintf("runtime-%d", i))
		for range 40 {
			require.NoError(t, logs.Append(cs, "", []*codespacev1.LogLine{line}))
		}
	}
	logs.mu.Lock()
	require.LessOrEqual(t, logs.bytes, maxManagerLogBytes)
	require.Greater(t, logs.bytes, maxManagerLogBytes-logbuffer.MaxBytes)
	for _, sender := range logs.senders {
		require.LessOrEqual(t, sender.buffer.Bytes(), logbuffer.MaxBytes)
	}
	logs.mu.Unlock()
	cs.UID = "runtime-0"
	cs.Status.Result = &api.OperationResult{Version: cs.Spec.Operation.Version, Succeeded: false}
	logs.CompleteOperation(cs)
	cs.Status.SettledOperationVersion = cs.Spec.Operation.Version
	require.NoError(t, logs.Append(cs, "", []*codespacev1.LogLine{line}))
	logs.mu.Lock()
	logs.senders[cs.UID].drainDeadline = time.Now().Add(-time.Second)
	logs.mu.Unlock()
	require.ErrorIs(t, logs.Append(cs, "", []*codespacev1.LogLine{line}), errCodespaceLogClosed)
	logs.CloseSite(cs.Spec.Site.UID)
	require.Zero(t, logs.bytes)
}

func TestManagerProgressUsesResourceFacts(t *testing.T) {
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Namespace: "site", UID: "runtime-uid"}, Spec: api.CodespaceSpec{Operation: api.Operation{Type: "create", Version: 2}}, Status: api.CodespaceStatus{Pod: api.ObservedResource{Name: "pod"}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: cs.Namespace, Labels: map[string]string{RuntimeUIDLabel: string(cs.UID)}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "runtime", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "credentials omitted"}}}}}}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).Build()
	_, message, err := managerProgress(t.Context(), kube, cs)
	require.NoError(t, err)
	require.Equal(t, "[INFO] [kubernetes] Waiting for container runtime: ErrImagePull", message)
	cs.Spec.Operation.Type = "stop"
	_, message, err = managerProgress(t.Context(), kube, cs)
	require.NoError(t, err)
	require.Equal(t, "[INFO] [kubernetes] Stopping runtime Pod", message)
}
