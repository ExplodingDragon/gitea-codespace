// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/controlplane"
)

const (
	operationLogBatchLines    = 64
	operationLogFlushInterval = 250 * time.Millisecond
	logGroupStartPrefix       = "##[group]"
	logGroupEnd               = "##[endgroup]"
	logErrorPrefix            = "##[error]"
)

var (
	logAuthorizationHeaderPattern = regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic)\s+)[^\s]+`)
	logBearerBasicPattern         = regexp.MustCompile(`(?i)\b((?:bearer|basic)\s+)[A-Za-z0-9._~+/=-]+`)
	logURLUserinfoPattern         = regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
)

type operationLogSink struct {
	agent           *Agent
	operation       *codespacev1.OperationPayload
	redactionValues []string
	mu              sync.Mutex
	pending         []*codespacev1.LogLine
	flushTimer      *time.Timer
	limitReached    bool
	groupDepth      int
}

func newOperationLogSink(agent *Agent, operation *codespacev1.OperationPayload) *operationLogSink {
	return &operationLogSink{agent: agent, operation: operation}
}

func (s *operationLogSink) WriteLifecycleLog(ctx context.Context, message string) error {
	if s == nil || s.agent == nil || s.operation == nil || message == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limitReached {
		return nil
	}
	s.pending = append(s.pending, &codespacev1.LogLine{
		TimestampUnixNano: time.Now().UnixNano(),
		Message:           s.agent.redactLogMessage(message, s.redactionValues...),
	})
	if len(s.pending) == 1 {
		flushCtx := context.WithoutCancel(ctx)
		s.flushTimer = time.AfterFunc(operationLogFlushInterval, func() {
			_ = s.FlushLifecycleLog(flushCtx)
		})
	}
	if len(s.pending) < operationLogBatchLines {
		return nil
	}
	return s.flushPendingLocked(ctx)
}

func (s *operationLogSink) startGroup(ctx context.Context, name string) error {
	if err := s.WriteLifecycleLog(ctx, logGroupStartPrefix+name); err != nil {
		return err
	}
	s.groupDepth++
	return nil
}

func (s *operationLogSink) endGroup(ctx context.Context) error {
	if s.groupDepth == 0 {
		return nil
	}
	if err := s.WriteLifecycleLog(ctx, logGroupEnd); err != nil {
		return err
	}
	s.groupDepth--
	return nil
}

func (s *operationLogSink) closeGroups(ctx context.Context) {
	for s.groupDepth > 0 {
		if s.endGroup(ctx) != nil {
			return
		}
	}
}

func (s *operationLogSink) FlushLifecycleLog(ctx context.Context) error {
	if s == nil || s.agent == nil || s.operation == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushPendingLocked(ctx)
}

func (s *operationLogSink) flushPendingLocked(ctx context.Context) error {
	if s.limitReached || len(s.pending) == 0 {
		return nil
	}
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	lines := s.pending
	s.pending = nil
	err := s.agent.updateLogLines(ctx, s.operation, lines)
	if failureCategory(err) == failureLogSizeExceeded {
		s.limitReached = true
		return nil
	}
	return err
}

func (a *Agent) updateLog(ctx context.Context, operation *codespacev1.OperationPayload, message string) error {
	err := a.updateLogLines(ctx, operation, []*codespacev1.LogLine{{
		TimestampUnixNano: time.Now().UnixNano(),
		Message:           a.redactLogMessage(message),
	}})
	if failureCategory(err) == failureLogSizeExceeded {
		return nil
	}
	return err
}

func (a *Agent) redactLogMessage(message string, redactionValues ...string) string {
	if secret := strings.TrimSpace(a.config.ManagerSecret); secret != "" {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	for _, secret := range redactionValues {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	message = logAuthorizationHeaderPattern.ReplaceAllString(message, "${1}[redacted]")
	message = logBearerBasicPattern.ReplaceAllString(message, "${1}[redacted]")
	message = logURLUserinfoPattern.ReplaceAllString(message, "${1}[redacted]@")
	return message
}

func (a *Agent) updateLogLines(ctx context.Context, operation *codespacev1.OperationPayload, lines []*codespacev1.LogLine) error {
	offset := operation.GetLogOffset()
	for len(lines) > 0 {
		count, err := a.updateLogBatchSize(operation, offset, lines)
		if err != nil {
			return err
		}
		request := connect.NewRequest(&codespacev1.UpdateLogRequest{
			ProtocolVersion:   controlplane.ProtocolVersion,
			CodespaceUuid:     operation.GetCodespaceUuid(),
			OperationRversion: operation.GetOperationRversion(),
			Offset:            offset,
			Lines:             lines[:count],
		})
		response, err := a.managerClient().UpdateLog(ctx, request)
		if err != nil {
			category := failureCategory(err)
			currentOffset, ok := logCurrentOffset(err)
			if ok && (category == failureLogOffsetConflict || category == failureLogOffsetGap) && currentOffset != offset {
				offset = currentOffset
				operation.LogOffset = currentOffset
				request.Msg.Offset = currentOffset
				response, err = a.managerClient().UpdateLog(ctx, request)
			}
			if err != nil {
				return fmt.Errorf("update log rpc: %w", err)
			}
		}
		offset = response.Msg.GetNextOffset()
		operation.LogOffset = offset
		lines = lines[count:]
	}
	return nil
}

func (a *Agent) updateLogBatchSize(operation *codespacev1.OperationPayload, offset int64, lines []*codespacev1.LogLine) (int, error) {
	if len(lines) == 0 {
		return 0, nil
	}
	request := connect.NewRequest(&codespacev1.UpdateLogRequest{
		ProtocolVersion:   controlplane.ProtocolVersion,
		CodespaceUuid:     operation.GetCodespaceUuid(),
		OperationRversion: operation.GetOperationRversion(),
		Offset:            offset,
		Lines:             lines[:1],
	})
	if err := a.checkControlPlaneMessageSize(request.Msg); err != nil {
		return 0, err
	}
	for count := 2; count <= len(lines); count++ {
		request.Msg.Lines = lines[:count]
		if err := a.checkControlPlaneMessageSize(request.Msg); err != nil {
			return count - 1, nil
		}
	}
	return len(lines), nil
}
