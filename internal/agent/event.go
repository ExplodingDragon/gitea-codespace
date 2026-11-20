// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"gitea.dev/codespace/internal/runtimeevent"
)

type RuntimeEventServer struct {
	Runtime *Runtime
	Remote  agentv1connect.AgentControlServiceClient
}

func (s *RuntimeEventServer) Emit(ctx context.Context, request *agentv1.EmitRequest) (*agentv1.EmitResponse, error) {
	if err := runtimeevent.Validate(request.Type, request.Message); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	s.Runtime.mu.RLock()
	redactor := s.Runtime.eventRedactor
	s.Runtime.mu.RUnlock()
	if redactor == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "runtime event credentials are not ready")
	}
	eventType := request.Type
	if redactor.RedactText(eventType) != eventType {
		eventType = "redacted"
	}
	message := redactor.RedactText(request.Message)
	if len(message) > runtimeevent.MaxMessageBytes {
		return nil, connect.NewError(connect.CodeResourceExhausted, "redacted event exceeds 16 KiB")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.Remote.ReportEvent(ctx, &agentv1.ReportEventRequest{Type: eventType, Message: message}); err != nil {
		return nil, err
	}
	return &agentv1.EmitResponse{}, nil
}
