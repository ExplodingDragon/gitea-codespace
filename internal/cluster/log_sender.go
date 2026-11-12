// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/logbuffer"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const maxManagerLogBytes = 64 * 1024 * 1024

var errOperationLogClosed = errors.New("operation log is closed")

// operationLogs owns the single Gitea sender for each active operation. All
// accounting is under mu; remote calls and retry delays are outside that lock.
type operationLogs struct {
	mu      sync.Mutex
	sites   map[types.UID]context.Context
	senders map[types.UID]*operationLogSender
	bytes   int
	remote  func(context.Context, api.ResourceReference) (codespacev1connect.ManagerServiceClient, error)
}

type operationLogSender struct {
	site        api.ResourceReference
	version     int64
	runtimeUUID string
	stage       string
	buffer      logbuffer.Buffer
	closed      bool
	finishAt    time.Time
	cancel      context.CancelFunc
	wake        chan struct{}
	done        chan struct{}
}

func (l *operationLogs) OpenSite(ctx context.Context, uid types.UID) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sites == nil {
		l.sites = make(map[types.UID]context.Context)
		l.senders = make(map[types.UID]*operationLogSender)
	}
	l.sites[uid] = ctx
}

func (l *operationLogs) CloseSite(uid types.UID) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.sites, uid)
	var done []<-chan struct{}
	for id, sender := range l.senders {
		if sender.site.UID == uid {
			sender.closed = true
			sender.cancel()
			done = append(done, sender.done)
			delete(l.senders, id)
		}
	}
	l.mu.Unlock()
	for _, ch := range done {
		<-ch
	}
}

// RetainSite uses the live inventory, so a lagging informer cannot discard a
// newly accepted operation's output.
func (l *operationLogs) RetainSite(uid types.UID, rows []api.Codespace) {
	if l == nil {
		return
	}
	active := make(map[types.UID]int64, len(rows))
	for i := range rows {
		cs := &rows[i]
		if cs.Spec.Site.UID == uid && cs.DeletionTimestamp.IsZero() && cs.Status.SettledOperationVersion < cs.Spec.Operation.Version && cs.Status.RecoveryAction == "" {
			active[cs.UID] = cs.Spec.Operation.Version
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, sender := range l.senders {
		if sender.site.UID == uid && active[id] != sender.version {
			sender.closed = true
			sender.cancel()
			delete(l.senders, id)
		}
	}
}

// Append accepts both infrastructure progress and Agent output. A stage dedups
// repeated observations in this process only; Agent batches use an empty stage.
func (l *operationLogs) Append(cs *api.Codespace, stage string, lines []*codespacev1.LogLine) error {
	if l == nil {
		return connect.NewError(connect.CodeUnavailable, "operation log sender is not ready")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx := l.sites[cs.Spec.Site.UID]
	if ctx == nil || ctx.Err() != nil {
		return connect.NewError(connect.CodeUnavailable, "operation log site is not active")
	}
	if !cs.Status.Bound || !cs.DeletionTimestamp.IsZero() || cs.Status.SettledOperationVersion >= cs.Spec.Operation.Version || cs.Status.RecoveryAction != "" {
		return errOperationLogClosed
	}
	sender := l.senders[cs.UID]
	if sender != nil && sender.version > cs.Spec.Operation.Version {
		return errOperationLogClosed
	}
	if sender == nil || sender.version != cs.Spec.Operation.Version {
		// Cached progress only supplements a live accepted operation. A stale
		// observation must not reopen a sender that live inventory has retired.
		if stage != "" {
			return nil
		}
		if sender != nil {
			sender.closed = true
			sender.cancel()
		}
		workerCtx, cancel := context.WithCancel(ctx)
		sender = &operationLogSender{site: cs.Spec.Site, runtimeUUID: cs.Spec.RuntimeUUID, version: cs.Spec.Operation.Version, stage: "accepted", cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
		l.senders[cs.UID] = sender
		l.bytes += sender.buffer.Add([]*codespacev1.LogLine{{TimestampUnixNano: time.Now().UnixNano(), Message: "Manager accepted the runtime operation"}}, maxManagerLogBytes-l.bytes)
		go l.send(workerCtx, sender)
	}
	if sender.closed || !sender.finishAt.IsZero() {
		return errOperationLogClosed
	}
	if stage == "" || stage != sender.stage {
		l.bytes += sender.buffer.Add(lines, maxManagerLogBytes-l.bytes)
		if stage != "" {
			sender.stage = stage
		}
	}
	select {
	case sender.wake <- struct{}{}:
	default:
	}
	return nil
}

// Finish is polled by the existing result-reporting loop. Logs get a bounded
// opportunity to drain without blocking finalization of another runtime.
func (l *operationLogs) Finish(cs *api.Codespace) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	sender := l.senders[cs.UID]
	if sender == nil || sender.version != cs.Spec.Operation.Version || sender.closed {
		return true
	}
	if sender.finishAt.IsZero() {
		sender.finishAt = time.Now().Add(5 * time.Second)
	}
	if sender.buffer.Bytes() == 0 || !time.Now().Before(sender.finishAt) {
		sender.closed = true
		sender.cancel()
		return true
	}
	select {
	case sender.wake <- struct{}{}:
	default:
	}
	return false
}

func (l *operationLogs) send(ctx context.Context, sender *operationLogSender) {
	defer close(sender.done)
	defer sender.cancel()
	defer func() {
		l.mu.Lock()
		l.bytes -= sender.buffer.Bytes()
		sender.buffer = logbuffer.Buffer{}
		sender.closed = true
		l.mu.Unlock()
	}()
	logger := log.FromContext(ctx).WithValues("site", sender.site.Name, "runtime", sender.runtimeUUID, "operation", sender.version)
	var offset int64
	knownOffset := false
	var lines []*codespacev1.LogLine
	var batchBytes int
	backoff := time.Second
	for ctx.Err() == nil {
		l.mu.Lock()
		closed := sender.closed
		if knownOffset && lines == nil && !closed {
			l.bytes += sender.buffer.Add(nil, maxManagerLogBytes-l.bytes)
			lines, batchBytes = sender.buffer.Take()
		}
		l.mu.Unlock()
		if closed {
			return
		}
		if knownOffset && len(lines) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-sender.wake:
			}
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		remote, err := l.remote(attempt, sender.site)
		var response *codespacev1.UpdateLogResponse
		if err == nil {
			response, err = remote.UpdateLog(attempt, &codespacev1.UpdateLogRequest{ProtocolVersion: 1, RuntimeUuid: sender.runtimeUUID, OperationRversion: sender.version, Offset: offset, Lines: lines})
		}
		cancel()
		if err == nil {
			if response == nil || response.NextOffset < offset || (len(lines) > 0 && response.NextOffset == offset) {
				logger.Error(errors.New("invalid log acknowledgement"), "Close operation log")
				return
			}
			offset, knownOffset, backoff = response.NextOffset, true, time.Second
			l.mu.Lock()
			sender.buffer.Release(batchBytes)
			l.bytes -= batchBytes
			l.mu.Unlock()
			lines, batchBytes = nil, 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		category := ""
		var rpcError *connect.Error
		if errors.As(err, &rpcError) {
			for _, detail := range rpcError.Details() {
				value, _ := connectproto.UnmarshalErrorDetail(detail)
				if failure, ok := value.(*codespacev1.FailureDetail); ok {
					category = failure.Category
					break
				}
			}
		}
		switch category {
		case "stale_operation", "codespace_not_found", "log_size_exceeded":
			return
		case "offset_conflict", "offset_gap":
			l.mu.Lock()
			sender.buffer.Release(batchBytes)
			sender.buffer.Drop(len(lines))
			l.bytes -= batchBytes
			l.mu.Unlock()
			lines, batchBytes, offset, knownOffset = nil, 0, 0, false
		default:
			switch connect.CodeOf(err) {
			case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeFailedPrecondition, connect.CodePermissionDenied, connect.CodeUnauthenticated, connect.CodeUnimplemented, connect.CodeOutOfRange:
				logger.Error(err, "Close operation log")
				return
			}
		}
		if backoff == time.Second {
			logger.Error(err, "Send operation log; retrying")
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(2*backoff, 5*time.Second)
	}
}
