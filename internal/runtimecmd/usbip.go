// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"context"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
)

// RunUSBIP asks the Runtime Agent to execute one bounded USB/IP operation.
func RunUSBIP(ctx context.Context, operation agentv1.RuntimeUSBIPOperation, argument, source string) (*agentv1.RuntimeUSBIPServiceRunResponse, error) {
	client := runtimeHTTPClient()
	defer client.CloseIdleConnections()
	remote := agentv1connect.NewRuntimeUSBIPServiceClient(connect.NewClient(connecthttp.NewTransport(client, "http://runtime-agent")))
	return remote.Run(ctx, &agentv1.RuntimeUSBIPServiceRunRequest{ProtocolVersion: 1, Operation: operation, Argument: argument, Source: source})
}
