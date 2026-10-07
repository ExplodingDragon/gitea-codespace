// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import "context"

type gatewayControlPlaneClient interface {
	ValidateOpenToken(context.Context, string, string, string) (gatewayOpenTokenDecision, error)
	ValidatePublicEndpoint(context.Context, string, string) (gatewayAccessDecision, error)
	VerifySSHPublicKey(context.Context, string, []byte) (gatewaySSHAuthDecision, error)
	RevalidateEndpointSession(context.Context, int64, string, string) (gatewayAccessDecision, error)
	RevalidateSSHSession(context.Context, int64, string) (gatewayAccessDecision, error)
}

type gatewayAccessDecision struct {
	Allowed        bool
	DeniedCategory string
}

// OpenTokenBinding is the user and runtime authorized by one-use browser code.
type OpenTokenBinding struct {
	UserID        int64
	CodespaceUUID string
	EndpointID    string
}

type gatewayOpenTokenDecision struct {
	Allowed        bool
	Binding        OpenTokenBinding
	deniedCategory string
}

type gatewaySSHAuthDecision struct {
	Allowed        bool
	UserID         int64
	deniedCategory string
}
