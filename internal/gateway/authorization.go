// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import "context"

type gatewayControlPlaneClient interface {
	ValidateOpenToken(context.Context, string, string, string) (gatewayOpenTokenDecision, error)
	ValidatePublicEndpoint(context.Context, string, string, string) (gatewayAccessDecision, error)
	VerifySSHPublicKey(context.Context, string, []byte) (gatewaySSHAuthDecision, error)
	RevalidateEndpointSession(context.Context, string, int64, string, string) (gatewayAccessDecision, error)
	RevalidateSSHSession(context.Context, string, int64, string) (gatewayAccessDecision, error)
}

type gatewayAccessDecision struct {
	Allowed        bool
	DeniedCategory string
}

// A browser open code authorizes exactly one user and Runtime binding.
type openTokenBinding struct {
	SiteUID       string
	UserID        int64
	CodespaceUUID string
	EndpointID    string
}

type gatewayOpenTokenDecision struct {
	Allowed        bool
	Binding        openTokenBinding
	deniedCategory string
}

type gatewaySSHAuthDecision struct {
	Allowed        bool
	SiteUID        string
	UserID         int64
	deniedCategory string
}
