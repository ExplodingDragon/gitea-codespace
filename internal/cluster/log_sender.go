// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/logbuffer"
	"gitea.dev/codespace/internal/runtimeevent"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const maxManagerLogBytes = 64 * 1024 * 1024

var errCodespaceLogClosed = errors.New("codespace log sender is closed")

// codespaceLogs owns the single Gitea sender for each instance's current operation. All
// accounting is under mu; remote calls and retry delays are outside that lock.
type codespaceLogs struct {
	mu                sync.Mutex
	sites             map[types.UID]context.Context
	senders           map[types.UID]*codespaceLogSender
	bytes             int
	eventMaxPerSecond int
	remote            func(context.Context, api.ResourceReference) (codespacev1connect.ManagerServiceClient, error)
}

type codespaceLogSender struct {
	site          api.ResourceReference
	version       int64
	codespaceID   int64
	progress      string
	buffer        logbuffer.Buffer
	closed        bool
	completed     bool
	eventTimes    []time.Time
	drainDeadline time.Time
	cancel        context.CancelFunc
	wake          chan struct{}
	done          chan struct{}
}

func (l *codespaceLogs) OpenSite(ctx context.Context, uid types.UID) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sites == nil {
		l.sites = make(map[types.UID]context.Context)
		l.senders = make(map[types.UID]*codespaceLogSender)
	}
	l.sites[uid] = ctx
}

func (l *codespaceLogs) CloseSite(uid types.UID) {
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
func (l *codespaceLogs) RetainSite(uid types.UID, rows []api.Codespace) {
	if l == nil {
		return
	}
	active := make(map[types.UID]int64, len(rows))
	for i := range rows {
		cs := &rows[i]
		if cs.Spec.Site.UID == uid && cs.DeletionTimestamp.IsZero() && cs.Status.RecoveryAction == "" {
			active[cs.UID] = cs.Spec.Operation.Version
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, sender := range l.senders {
		if sender.site.UID == uid && active[id] != sender.version {
			sender.closed = true
			sender.cancel()
			// Keep the rate window across operation changes and reject stale
			// observations until the new operation opens its sender.
			if _, exists := active[id]; !exists {
				delete(l.senders, id)
			}
		}
	}
}

// Append deduplicates infrastructure progress independently of diagnostics.
// Agent batches use an empty stage; diagnostic captures own their deduplication.
func (l *codespaceLogs) Append(cs *api.Codespace, stage string, lines []*codespacev1.LogLine) error {
	if l == nil {
		return connect.NewError(connect.CodeUnavailable, "codespace log sender is not ready")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	sender, err := l.senderLocked(cs, stage, false)
	if err != nil || sender == nil {
		return err
	}
	if stage == "" || stage == diagnosticLogStage || stage != sender.progress {
		l.bytes += sender.buffer.Add(lines, maxManagerLogBytes-l.bytes)
		if stage != "" && stage != diagnosticLogStage {
			sender.progress = stage
		}
	}
	select {
	case sender.wake <- struct{}{}:
	default:
	}
	return nil
}

// senderLocked also recovers a running instance's event sender after election.
// Ordinary output cannot reopen a settled operation by itself.
func (l *codespaceLogs) senderLocked(cs *api.Codespace, stage string, event bool) (*codespaceLogSender, error) {
	ctx := l.sites[cs.Spec.Site.UID]
	if ctx == nil || ctx.Err() != nil {
		return nil, connect.NewError(connect.CodeUnavailable, "codespace log site is not active")
	}
	if !cs.DeletionTimestamp.IsZero() || cs.Status.RecoveryAction != "" {
		return nil, errCodespaceLogClosed
	}
	sender := l.senders[cs.UID]
	if sender != nil && sender.version > cs.Spec.Operation.Version {
		return nil, errCodespaceLogClosed
	}
	if sender == nil || sender.version != cs.Spec.Operation.Version {
		settled := cs.Status.SettledOperationVersion >= cs.Spec.Operation.Version
		if settled && !(event && hasRunningRuntime(cs)) {
			return nil, errCodespaceLogClosed
		}
		// Cached progress only supplements a live accepted operation. A stale
		// observation must not reopen a sender that live inventory has retired.
		if stage != "" {
			return nil, nil
		}
		var eventTimes []time.Time
		if sender != nil {
			sender.closed = true
			sender.cancel()
			eventTimes = sender.eventTimes
		}
		workerCtx, cancel := context.WithCancel(ctx)
		sender = &codespaceLogSender{
			site: cs.Spec.Site, codespaceID: cs.Spec.CodespaceID, version: cs.Spec.Operation.Version,
			completed: settled, eventTimes: eventTimes,
			cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}),
		}
		l.senders[cs.UID] = sender
		if !settled {
			l.bytes += sender.buffer.Add([]*codespacev1.LogLine{{TimestampUnixNano: time.Now().UnixNano(), Message: fmt.Sprintf("[INFO] [operation] Accepted %s #%d", strings.TrimPrefix(cs.Spec.Operation.Type, "abort_"), sender.version)}}, maxManagerLogBytes-l.bytes)
		}
		go l.send(workerCtx, sender)
	}
	if sender.closed || (!sender.drainDeadline.IsZero() && !time.Now().Before(sender.drainDeadline)) {
		return nil, errCodespaceLogClosed
	}
	return sender, nil
}

func (l *codespaceLogs) ReportEvent(cs *api.Codespace, eventType, message string) error {
	if err := runtimeevent.Validate(eventType, message); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if l == nil {
		return connect.NewError(connect.CodeUnavailable, "codespace log sender is not ready")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	sender, err := l.senderLocked(cs, "", true)
	if err != nil {
		if errors.Is(err, errCodespaceLogClosed) {
			return connect.NewError(connect.CodeFailedPrecondition, err.Error())
		}
		return err
	}
	cutoff := now.Add(-time.Second)
	first := 0
	for first < len(sender.eventTimes) && !sender.eventTimes[first].After(cutoff) {
		first++
	}
	sender.eventTimes = slices.Delete(sender.eventTimes, 0, first)
	if len(sender.eventTimes) >= l.eventMaxPerSecond {
		return connect.NewError(connect.CodeResourceExhausted, "codespace event rate limit exceeded")
	}
	var lines []*codespacev1.LogLine
	for _, line := range strings.Split(strings.ReplaceAll(message, "\r", ""), "\n") {
		lines = append(lines, &codespacev1.LogLine{TimestampUnixNano: now.UnixNano(), Message: "[EVENT] [" + eventType + "] " + line})
	}
	size, accepted := sender.buffer.TryAdd(lines, maxManagerLogBytes-l.bytes)
	if !accepted {
		return connect.NewError(connect.CodeResourceExhausted, "codespace log queue is full")
	}
	l.bytes += size
	sender.eventTimes = append(sender.eventTimes, now)
	select {
	case sender.wake <- struct{}{}:
	default:
	}
	return nil
}

func hasRunningRuntime(cs *api.Codespace) bool {
	result := cs.Status.Result
	return (cs.Spec.Operation.Type == "create" || cs.Spec.Operation.Type == "resume") &&
		result != nil && result.Version == cs.Spec.Operation.Version && result.Succeeded &&
		cs.Status.Target != nil && cs.Status.Target.Ready && cs.Status.Pod.UID != ""
}

// CompleteOperation records the summary once. A running instance retains its
// sender; terminal operations allow only a short, bounded output drain.
func (l *codespaceLogs) CompleteOperation(cs *api.Codespace) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	sender := l.senders[cs.UID]
	if sender == nil || sender.version != cs.Spec.Operation.Version || sender.closed || sender.completed {
		return
	}
	result := cs.Status.Result
	if result == nil || result.Version != sender.version {
		return
	}
	level, outcome := "INFO", "completed"
	if !result.Succeeded {
		level, outcome = "ERROR", "failed; see preceding output"
	} else if cs.Spec.Operation.Type == "stop" {
		outcome = "completed; workspace volume retained"
	} else if cs.Spec.Operation.Type == "delete" {
		outcome = "completed; runtime resources removed"
	}
	message := fmt.Sprintf("[%s] [operation] %s #%d %s", level, strings.TrimPrefix(cs.Spec.Operation.Type, "abort_"), sender.version, outcome)
	l.bytes += sender.buffer.Add([]*codespacev1.LogLine{{TimestampUnixNano: time.Now().UnixNano(), Message: message}}, maxManagerLogBytes-l.bytes)
	sender.completed = true
	if !hasRunningRuntime(cs) {
		sender.drainDeadline = time.Now().Add(5 * time.Second)
		time.AfterFunc(time.Until(sender.drainDeadline), sender.cancel)
	}
	select {
	case sender.wake <- struct{}{}:
	default:
	}
}

func (l *codespaceLogs) send(ctx context.Context, sender *codespaceLogSender) {
	defer close(sender.done)
	defer sender.cancel()
	defer func() {
		l.mu.Lock()
		l.bytes -= sender.buffer.Bytes()
		sender.buffer = logbuffer.Buffer{}
		sender.closed = true
		l.mu.Unlock()
	}()
	logger := log.FromContext(ctx).WithValues("site", sender.site.Name, "runtime", sender.codespaceID, "operation", sender.version)
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
			response, err = remote.UpdateLog(attempt, &codespacev1.UpdateLogRequest{CodespaceId: sender.codespaceID, OperationRversion: sender.version, Offset: offset, Lines: lines})
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
