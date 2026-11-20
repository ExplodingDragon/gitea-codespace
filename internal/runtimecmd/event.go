// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"gitea.dev/codespace/internal/runtimeevent"
)

// EmitEvent returns after Manager accepts the event, without replaying requests.
func EmitEvent(ctx context.Context, eventType, message string) error {
	if err := runtimeevent.Validate(eventType, message); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := runtimeHTTPClient()
	defer client.CloseIdleConnections()
	remote := agentv1connect.NewRuntimeEventServiceClient(connect.NewClient(connecthttp.NewTransport(client, "http://runtime-agent")))
	_, err := remote.Emit(ctx, &agentv1.EmitRequest{Type: eventType, Message: message})
	return err
}
