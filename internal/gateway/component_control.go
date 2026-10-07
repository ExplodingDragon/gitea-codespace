// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	componentv1 "gitea.dev/codespace-proto-go/component/v1"
	"gitea.dev/codespace-proto-go/component/v1/componentv1connect"
)

// ComponentControlPlane delegates Gitea authorization to Manager, which owns
// the site credentials. Gateway receives only the authorization outcome.
type ComponentControlPlane struct {
	Client componentv1connect.ComponentServiceClient
}

func (c *ComponentControlPlane) authorize(ctx context.Context, payload *componentv1.AuthorizeGatewayRequest) (*componentv1.AuthorizeGatewayResponse, error) {
	if c == nil || c.Client == nil {
		return nil, fmt.Errorf("gateway component control plane is unavailable")
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload.ProtocolVersion = 1
	response, err := c.Client.AuthorizeGateway(callCtx, connect.NewRequest(payload))
	if err != nil {
		return nil, err
	}
	if response.Msg.Allowed == (response.Msg.DeniedCategory != "") {
		return nil, fmt.Errorf("manager returned an invalid Gateway authorization outcome")
	}
	return response.Msg, nil
}

func (c *ComponentControlPlane) ValidateOpenToken(ctx context.Context, code, runtimeUUID, endpointID string) (gatewayOpenTokenDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_OpenCode{OpenCode: &componentv1.OpenCodeAuthorization{Code: code, RuntimeUuid: runtimeUUID, EndpointId: endpointID}}})
	if err != nil {
		return gatewayOpenTokenDecision{}, err
	}
	if !result.Allowed {
		return gatewayOpenTokenDecision{deniedCategory: result.DeniedCategory}, nil
	}
	return gatewayOpenTokenDecision{Allowed: true, Binding: OpenTokenBinding{UserID: result.UserId, CodespaceUUID: result.RuntimeUuid, EndpointID: result.EndpointId}}, nil
}

func (c *ComponentControlPlane) ValidatePublicEndpoint(ctx context.Context, runtimeUUID, endpointID string) (gatewayAccessDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_PublicEndpoint{PublicEndpoint: &componentv1.PublicEndpointAuthorization{RuntimeUuid: runtimeUUID, EndpointId: endpointID}}})
	if err != nil {
		return gatewayAccessDecision{}, err
	}
	return gatewayAccessDecision{Allowed: result.Allowed, DeniedCategory: result.DeniedCategory}, nil
}

func (c *ComponentControlPlane) VerifySSHPublicKey(ctx context.Context, runtimeUUID string, publicKey []byte) (gatewaySSHAuthDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_SshPublicKey{SshPublicKey: &componentv1.SSHPublicKeyAuthorization{RuntimeUuid: runtimeUUID, PublicKey: append([]byte(nil), publicKey...)}}})
	if err != nil {
		return gatewaySSHAuthDecision{}, err
	}
	return gatewaySSHAuthDecision{Allowed: result.Allowed, UserID: result.UserId, deniedCategory: result.DeniedCategory}, nil
}

func (c *ComponentControlPlane) RevalidateEndpointSession(ctx context.Context, userID int64, runtimeUUID, endpointID string) (gatewayAccessDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_EndpointSession{EndpointSession: &componentv1.EndpointSessionAuthorization{UserId: userID, RuntimeUuid: runtimeUUID, EndpointId: endpointID}}})
	if err != nil {
		return gatewayAccessDecision{}, err
	}
	return gatewayAccessDecision{Allowed: result.Allowed, DeniedCategory: result.DeniedCategory}, nil
}

func (c *ComponentControlPlane) RevalidateSSHSession(ctx context.Context, userID int64, runtimeUUID string) (gatewayAccessDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_SshSession{SshSession: &componentv1.SSHSessionAuthorization{UserId: userID, RuntimeUuid: runtimeUUID}}})
	if err != nil {
		return gatewayAccessDecision{}, err
	}
	return gatewayAccessDecision{Allowed: result.Allowed, DeniedCategory: result.DeniedCategory}, nil
}

var _ gatewayControlPlaneClient = (*ComponentControlPlane)(nil)
