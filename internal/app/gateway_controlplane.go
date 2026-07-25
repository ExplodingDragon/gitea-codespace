// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/manager"
	"google.golang.org/protobuf/proto"
)

const (
	gatewayManagerIDHeader     = "x-codespace-manager-id"
	gatewayManagerSecretHeader = "x-codespace-manager-secret"
	gatewayProtocolVersion     = 1
)

type gatewayControlPlane struct {
	baseURL        string
	httpClient     connect.HTTPClient
	mu             sync.RWMutex
	client         codespacev1connect.ManagerServiceClient
	managerID      int64
	managerSecret  string
	maxMessageSize int64
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
		baseURL:       baseURL,
		httpClient:    httpClient,
		client:        codespacev1connect.NewManagerServiceClient(httpClient, baseURL),
		managerID:     managerID,
		managerSecret: managerSecret,
	}
}

func (c *gatewayControlPlane) SaveManagerServiceSettings(settings manager.ManagerServiceSettings) error {
	c.mu.Lock()
	if settings.ControlPlaneMaxMessageSize > 0 {
		c.client = newGatewayManagerServiceClient(c.httpClient, c.baseURL, settings.ControlPlaneMaxMessageSize)
		c.maxMessageSize = settings.ControlPlaneMaxMessageSize
	}
	c.mu.Unlock()
	return nil
}

func newGatewayManagerServiceClient(httpClient connect.HTTPClient, baseURL string, maxBytes int64) codespacev1connect.ManagerServiceClient {
	opts := []connect.ClientOption(nil)
	if maxBytes > 0 {
		max := int(maxBytes)
		if int64(max) != maxBytes {
			max = math.MaxInt
		}
		opts = append(opts, connect.WithReadMaxBytes(max), connect.WithSendMaxBytes(max))
	}
	return codespacev1connect.NewManagerServiceClient(httpClient, baseURL, opts...)
}

func (c *gatewayControlPlane) managerClient() codespacev1connect.ManagerServiceClient {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

func (c *gatewayControlPlane) checkMessageSize(message proto.Message) error {
	c.mu.RLock()
	maxSize := c.maxMessageSize
	c.mu.RUnlock()
	if maxSize <= 0 || message == nil {
		return nil
	}
	size := proto.Size(message)
	if int64(size) <= maxSize {
		return nil
	}
	return fmt.Errorf("control plane message size %d exceeds limit %d", size, maxSize)
}

func (c *gatewayControlPlane) validateOpenToken(ctx context.Context, code string) (gatewayOpenTokenDecision, error) {
	request := connect.NewRequest(&codespacev1.ValidateOpenTokenRequest{
		ProtocolVersion: gatewayProtocolVersion,
		Code:            code,
	})
	c.setManagerAuth(request.Header())
	response, err := c.managerClient().ValidateOpenToken(ctx, request)
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
	response, err := c.managerClient().ValidatePublicEndpoint(ctx, request)
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
	response, err := c.managerClient().VerifySSHPublicKey(ctx, request)
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

func (c *gatewayControlPlane) ensureCodespaceGitSSHKey(ctx context.Context, codespaceUUID string, publicKey []byte) ([]string, error) {
	request := connect.NewRequest(&codespacev1.EnsureCodespaceGitSSHKeyRequest{
		ProtocolVersion: gatewayProtocolVersion,
		CodespaceUuid:   codespaceUUID,
		PublicKey:       append([]byte(nil), publicKey...),
	})
	c.setManagerAuth(request.Header())
	response, err := c.managerClient().EnsureCodespaceGitSSHKey(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("ensure codespace git ssh key rpc: %w", err)
	}
	return append([]string(nil), response.Msg.GetKnownHostsLines()...), nil
}

func (c *gatewayControlPlane) reportRuntimeMetadata(ctx context.Context, codespaceUUID string, metadata *codespacev1.RuntimeMetadata, metadataGeneration int64) error {
	request := connect.NewRequest(&codespacev1.ReportRuntimeMetadataRequest{
		ProtocolVersion:    gatewayProtocolVersion,
		CodespaceUuid:      codespaceUUID,
		MetadataGeneration: metadataGeneration,
		Metadata:           metadata,
	})
	c.setManagerAuth(request.Header())
	if err := c.checkMessageSize(request.Msg); err != nil {
		return err
	}
	if _, err := c.managerClient().ReportRuntimeMetadata(ctx, request); err != nil {
		return fmt.Errorf("report runtime metadata rpc: %w", err)
	}
	return nil
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
	response, err := c.managerClient().RevalidateGatewaySession(ctx, request)
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
