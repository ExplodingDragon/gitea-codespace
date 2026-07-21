// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

const (
	gatewayManagerIDHeader     = "x-codespace-manager-id"
	gatewayManagerSecretHeader = "x-codespace-manager-secret"
	gatewayProtocolVersion     = 1
)

type gatewayControlPlane struct {
	client        codespacev1connect.ManagerServiceClient
	managerID     int64
	managerSecret string
}

type gatewayAccessDecision struct {
	allowed        bool
	deniedCategory string
}

type gatewayOpenTokenBinding struct {
	userID                int64
	codespaceUUID         string
	endpointID            string
	interactionGeneration int64
}

type gatewayOpenTokenDecision struct {
	allowed        bool
	binding        gatewayOpenTokenBinding
	deniedCategory string
}

type gatewaySSHAuthDecision struct {
	allowed               bool
	userID                int64
	interactionGeneration int64
	deniedCategory        string
}

func newGatewayControlPlane(baseURL string, managerID int64, managerSecret string, httpClient *http.Client) *gatewayControlPlane {
	return &gatewayControlPlane{
		client:        codespacev1connect.NewManagerServiceClient(httpClient, baseURL),
		managerID:     managerID,
		managerSecret: managerSecret,
	}
}

func (c *gatewayControlPlane) validateOpenToken(ctx context.Context, code string) (gatewayOpenTokenDecision, error) {
	request := connect.NewRequest(&codespacev1.ValidateOpenTokenRequest{
		ProtocolVersion: gatewayProtocolVersion,
		Code:            code,
	})
	c.setManagerAuth(request.Header())
	response, err := c.client.ValidateOpenToken(ctx, request)
	if err != nil {
		return gatewayOpenTokenDecision{}, fmt.Errorf("validate open token rpc: %w", err)
	}
	if allowed := response.Msg.GetAllowed(); allowed != nil {
		return gatewayOpenTokenDecision{
			allowed: true,
			binding: gatewayOpenTokenBinding{
				userID:                allowed.GetUserId(),
				codespaceUUID:         allowed.GetCodespaceUuid(),
				endpointID:            allowed.GetEndpointId(),
				interactionGeneration: allowed.GetInteractionGeneration(),
			},
		}, nil
	}
	if denied := response.Msg.GetDenied(); denied != nil {
		return gatewayOpenTokenDecision{deniedCategory: denied.GetCategory()}, nil
	}
	return gatewayOpenTokenDecision{}, fmt.Errorf("validate open token outcome is missing")
}

func (c *gatewayControlPlane) validatePublicEndpoint(ctx context.Context, codespaceUUID, endpointID string) (gatewayAccessDecision, error) {
	request := connect.NewRequest(&codespacev1.ValidatePublicEndpointRequest{
		ProtocolVersion: gatewayProtocolVersion,
		CodespaceUuid:   codespaceUUID,
		EndpointId:      endpointID,
	})
	c.setManagerAuth(request.Header())
	response, err := c.client.ValidatePublicEndpoint(ctx, request)
	if err != nil {
		return gatewayAccessDecision{}, fmt.Errorf("validate public endpoint rpc: %w", err)
	}
	return gatewayAccessDecisionFromOutcome(
		response.Msg.GetAllowed() != nil,
		response.Msg.GetDenied(),
		"validate public endpoint",
	)
}

func (c *gatewayControlPlane) verifySSHPublicKey(ctx context.Context, codespaceUUID string, publicKey []byte) (gatewaySSHAuthDecision, error) {
	request := connect.NewRequest(&codespacev1.VerifySSHPublicKeyRequest{
		ProtocolVersion: gatewayProtocolVersion,
		CodespaceUuid:   codespaceUUID,
		PublicKey:       append([]byte(nil), publicKey...),
	})
	c.setManagerAuth(request.Header())
	response, err := c.client.VerifySSHPublicKey(ctx, request)
	if err != nil {
		return gatewaySSHAuthDecision{}, fmt.Errorf("verify ssh public key rpc: %w", err)
	}
	if allowed := response.Msg.GetAllowed(); allowed != nil {
		return gatewaySSHAuthDecision{
			allowed:               true,
			userID:                allowed.GetUserId(),
			interactionGeneration: allowed.GetInteractionGeneration(),
		}, nil
	}
	if denied := response.Msg.GetDenied(); denied != nil {
		return gatewaySSHAuthDecision{deniedCategory: denied.GetCategory()}, nil
	}
	return gatewaySSHAuthDecision{}, fmt.Errorf("verify ssh public key outcome is missing")
}

func (c *gatewayControlPlane) revalidateEndpointSession(ctx context.Context, userID int64, codespaceUUID, endpointID string) (gatewayAccessDecision, error) {
	return c.revalidateGatewaySession(ctx, &codespacev1.RevalidateGatewaySessionRequest{
		ProtocolVersion: gatewayProtocolVersion,
		Session: &codespacev1.RevalidateGatewaySessionRequest_Endpoint{
			Endpoint: &codespacev1.EndpointSessionBinding{
				UserId:        userID,
				CodespaceUuid: codespaceUUID,
				EndpointId:    endpointID,
			},
		},
	})
}

func (c *gatewayControlPlane) revalidateSSHSession(ctx context.Context, userID int64, codespaceUUID string) (gatewayAccessDecision, error) {
	return c.revalidateGatewaySession(ctx, &codespacev1.RevalidateGatewaySessionRequest{
		ProtocolVersion: gatewayProtocolVersion,
		Session: &codespacev1.RevalidateGatewaySessionRequest_Ssh{
			Ssh: &codespacev1.SSHSessionBinding{
				UserId:        userID,
				CodespaceUuid: codespaceUUID,
			},
		},
	})
}

func (c *gatewayControlPlane) revalidateGatewaySession(ctx context.Context, payload *codespacev1.RevalidateGatewaySessionRequest) (gatewayAccessDecision, error) {
	request := connect.NewRequest(payload)
	c.setManagerAuth(request.Header())
	response, err := c.client.RevalidateGatewaySession(ctx, request)
	if err != nil {
		return gatewayAccessDecision{}, fmt.Errorf("revalidate gateway session rpc: %w", err)
	}
	return gatewayAccessDecisionFromOutcome(
		response.Msg.GetAllowed() != nil,
		response.Msg.GetDenied(),
		"revalidate gateway session",
	)
}

func gatewayAccessDecisionFromOutcome(allowed bool, denied *codespacev1.FailureDetail, rpc string) (gatewayAccessDecision, error) {
	if allowed {
		return gatewayAccessDecision{allowed: true}, nil
	}
	if denied != nil {
		return gatewayAccessDecision{deniedCategory: denied.GetCategory()}, nil
	}
	return gatewayAccessDecision{}, fmt.Errorf("%s outcome is missing", rpc)
}

func (c *gatewayControlPlane) setManagerAuth(header http.Header) {
	header.Set(gatewayManagerIDHeader, fmt.Sprintf("%d", c.managerID))
	header.Set(gatewayManagerSecretHeader, c.managerSecret)
}
