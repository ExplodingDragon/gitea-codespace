// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"fmt"

	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"gitea.dev/codespace/internal/rpc/component/v1/componentv1connect"
)

// The component control plane delegates Gitea authorization to Manager, which
// owns the site credentials. Gateway receives only the authorization outcome.
type componentControlPlane struct {
	Client componentv1connect.ComponentServiceClient
}

func (c *componentControlPlane) authorize(ctx context.Context, payload *componentv1.AuthorizeGatewayRequest) (*componentv1.AuthorizeGatewayResponse, error) {
	if c == nil || c.Client == nil {
		return nil, fmt.Errorf("gateway component control plane is unavailable")
	}
	callCtx, cancel := context.WithTimeout(ctx, gatewayAuthorizationTimeout)
	defer cancel()
	payload.ProtocolVersion = 1
	response, err := c.Client.AuthorizeGateway(callCtx, payload)
	if err != nil {
		return nil, err
	}
	if response.Allowed == (response.DeniedCategory != "") {
		return nil, fmt.Errorf("manager returned an invalid Gateway authorization outcome")
	}
	if response.Allowed && response.SiteUid == "" {
		return nil, fmt.Errorf("manager returned an authorization without a site identity")
	}
	return response, nil
}

func (c *componentControlPlane) ValidateOpenToken(ctx context.Context, code, runtimeUUID, endpointID string) (gatewayOpenTokenDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_OpenCode{OpenCode: &componentv1.OpenCodeAuthorization{Code: code, RuntimeUuid: runtimeUUID, EndpointId: endpointID}}})
	if err != nil {
		return gatewayOpenTokenDecision{}, err
	}
	if !result.Allowed {
		return gatewayOpenTokenDecision{deniedCategory: result.DeniedCategory}, nil
	}
	return gatewayOpenTokenDecision{Allowed: true, Binding: openTokenBinding{SiteUID: result.SiteUid, UserID: result.UserId, CodespaceUUID: result.RuntimeUuid, EndpointID: result.EndpointId}}, nil
}

func (c *componentControlPlane) ValidatePublicEndpoint(ctx context.Context, siteUID, runtimeUUID, endpointID string) (gatewayAccessDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_PublicEndpoint{PublicEndpoint: &componentv1.PublicEndpointAuthorization{RuntimeUuid: runtimeUUID, EndpointId: endpointID}}})
	if err != nil {
		return gatewayAccessDecision{}, err
	}
	if result.Allowed && result.SiteUid != siteUID {
		return gatewayAccessDecision{}, fmt.Errorf("manager returned a different site identity")
	}
	return gatewayAccessDecision{Allowed: result.Allowed, DeniedCategory: result.DeniedCategory}, nil
}

func (c *componentControlPlane) VerifySSHPublicKey(ctx context.Context, runtimeUUID string, publicKey []byte) (gatewaySSHAuthDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_SshPublicKey{SshPublicKey: &componentv1.SSHPublicKeyAuthorization{RuntimeUuid: runtimeUUID, PublicKey: append([]byte(nil), publicKey...)}}})
	if err != nil {
		return gatewaySSHAuthDecision{}, err
	}
	return gatewaySSHAuthDecision{Allowed: result.Allowed, SiteUID: result.SiteUid, UserID: result.UserId, deniedCategory: result.DeniedCategory}, nil
}

func (c *componentControlPlane) RevalidateEndpointSession(ctx context.Context, siteUID string, userID int64, runtimeUUID, endpointID string) (gatewayAccessDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_EndpointSession{EndpointSession: &componentv1.EndpointSessionAuthorization{SiteUid: siteUID, UserId: userID, RuntimeUuid: runtimeUUID, EndpointId: endpointID}}})
	if err != nil {
		return gatewayAccessDecision{}, err
	}
	if result.Allowed && result.SiteUid != siteUID {
		return gatewayAccessDecision{}, fmt.Errorf("manager returned a different site identity")
	}
	return gatewayAccessDecision{Allowed: result.Allowed, DeniedCategory: result.DeniedCategory}, nil
}

func (c *componentControlPlane) RevalidateSSHSession(ctx context.Context, siteUID string, userID int64, runtimeUUID string) (gatewayAccessDecision, error) {
	result, err := c.authorize(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_SshSession{SshSession: &componentv1.SSHSessionAuthorization{SiteUid: siteUID, UserId: userID, RuntimeUuid: runtimeUUID}}})
	if err != nil {
		return gatewayAccessDecision{}, err
	}
	if result.Allowed && result.SiteUid != siteUID {
		return gatewayAccessDecision{}, fmt.Errorf("manager returned a different site identity")
	}
	return gatewayAccessDecision{Allowed: result.Allowed, DeniedCategory: result.DeniedCategory}, nil
}

var _ gatewayControlPlaneClient = (*componentControlPlane)(nil)
