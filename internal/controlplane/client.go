// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package controlplane

import (
	"context"
	"runtime/debug"
	"strconv"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

// BuildVersion identifies the binary independently of the wire protocol version.
func BuildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "development"
}

const (
	// ProtocolVersion is the ManagerService protocol implemented by this binary.
	ProtocolVersion int32 = 1
	// ManagerIDHeader carries the Gitea-issued Manager identity.
	ManagerIDHeader = "x-codespace-manager-id"
	// ManagerSecretHeader authenticates the Gitea-issued Manager identity.
	ManagerSecretHeader = "x-codespace-manager-secret"
)

// NewManagerServiceClient returns an authenticated ManagerService client.
func NewManagerServiceClient(httpClient connecthttp.HTTPClient, baseURL string, managerID int64, managerSecret string) codespacev1connect.ManagerServiceClient {
	client := connect.NewClient(connecthttp.NewTransport(httpClient, baseURL,
		connecthttp.WithReadMaxBytes(codespacev1.ManagerServiceMaxMessageBytes),
		connecthttp.WithSendMaxBytes(codespacev1.ManagerServiceMaxMessageBytes)), func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			info, _ := connect.CallInfoForClientContext(ctx)
			info.RequestHeader().Set(ManagerIDHeader, strconv.FormatInt(managerID, 10))
			info.RequestHeader().Set(ManagerSecretHeader, managerSecret)
			return next(ctx, spec)
		}
	})
	return codespacev1connect.NewManagerServiceClient(client)
}
