// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/controlplane"
	"gitea.dev/codespace/internal/manager"
	"google.golang.org/protobuf/proto"
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

type gatewayControlPlaneClient interface {
	validateOpenToken(context.Context, string) (gatewayOpenTokenDecision, error)
	validatePublicEndpoint(context.Context, string, string) (gatewayAccessDecision, error)
	verifySSHPublicKey(context.Context, string, []byte) (gatewaySSHAuthDecision, error)
	revalidateEndpointSession(context.Context, int64, string, string) (gatewayAccessDecision, error)
	revalidateSSHSession(context.Context, int64, string) (gatewayAccessDecision, error)
}

type gatewayControlPlanePool struct {
	routes *gatewayRouteStore
	sites  map[int64]*gatewayControlPlane
}

func newGatewayControlPlanePool(routes *gatewayRouteStore) *gatewayControlPlanePool {
	return &gatewayControlPlanePool{routes: routes, sites: make(map[int64]*gatewayControlPlane)}
}

func (p *gatewayControlPlanePool) Add(siteID int64, controlPlane *gatewayControlPlane) {
	if p != nil && siteID > 0 && controlPlane != nil {
		p.sites[siteID] = controlPlane
	}
}

func (p *gatewayControlPlanePool) forCodespace(codespaceUUID string) (*gatewayControlPlane, error) {
	if p == nil || p.routes == nil {
		return nil, fmt.Errorf("gateway control plane is unavailable")
	}
	siteID, ok := p.routes.siteForCodespace(codespaceUUID)
	if !ok || p.sites[siteID] == nil {
		return nil, fmt.Errorf("gateway control plane for codespace is unavailable")
	}
	return p.sites[siteID], nil
}

func (p *gatewayControlPlanePool) validateOpenToken(ctx context.Context, code string) (gatewayOpenTokenDecision, error) {
	var denied gatewayOpenTokenDecision
	var lastErr error
	failed := 0
	for _, controlPlane := range p.sites {
		decision, err := controlPlane.validateOpenToken(ctx, code)
		if err != nil {
			lastErr = err
			failed++
			continue
		}
		if decision.allowed {
			return decision, nil
		}
		denied = decision
	}
	if failed == len(p.sites) && lastErr != nil {
		return gatewayOpenTokenDecision{}, lastErr
	}
	return denied, nil
}

func (p *gatewayControlPlanePool) validatePublicEndpoint(ctx context.Context, codespaceUUID, endpointID string) (gatewayAccessDecision, error) {
	controlPlane, err := p.forCodespace(codespaceUUID)
	if err != nil {
		return gatewayAccessDecision{}, err
	}
	return controlPlane.validatePublicEndpoint(ctx, codespaceUUID, endpointID)
}

func (p *gatewayControlPlanePool) verifySSHPublicKey(ctx context.Context, codespaceUUID string, key []byte) (gatewaySSHAuthDecision, error) {
	controlPlane, err := p.forCodespace(codespaceUUID)
	if err != nil {
		return gatewaySSHAuthDecision{}, err
	}
	return controlPlane.verifySSHPublicKey(ctx, codespaceUUID, key)
}

func (p *gatewayControlPlanePool) revalidateEndpointSession(ctx context.Context, userID int64, codespaceUUID, endpointID string) (gatewayAccessDecision, error) {
	controlPlane, err := p.forCodespace(codespaceUUID)
	if err != nil {
		return gatewayAccessDecision{}, err
	}
	return controlPlane.revalidateEndpointSession(ctx, userID, codespaceUUID, endpointID)
}

func (p *gatewayControlPlanePool) revalidateSSHSession(ctx context.Context, userID int64, codespaceUUID string) (gatewayAccessDecision, error) {
	controlPlane, err := p.forCodespace(codespaceUUID)
	if err != nil {
		return gatewayAccessDecision{}, err
	}
	return controlPlane.revalidateSSHSession(ctx, userID, codespaceUUID)
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
		client:        controlplane.NewManagerServiceClient(httpClient, baseURL, managerID, managerSecret, 0),
		managerID:     managerID,
		managerSecret: managerSecret,
	}
}

func (c *gatewayControlPlane) SaveManagerServiceSettings(settings manager.ManagerServiceSettings) error {
	c.mu.Lock()
	if settings.ControlPlaneMaxMessageSize > 0 {
		c.client = controlplane.NewManagerServiceClient(c.httpClient, c.baseURL, c.managerID, c.managerSecret, settings.ControlPlaneMaxMessageSize)
		c.maxMessageSize = settings.ControlPlaneMaxMessageSize
	}
	c.mu.Unlock()
	return nil
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
	return controlplane.CheckMessageSize(message, maxSize)
}

func (c *gatewayControlPlane) validateOpenToken(ctx context.Context, code string) (gatewayOpenTokenDecision, error) {
	request := connect.NewRequest(&codespacev1.ValidateOpenTokenRequest{
		ProtocolVersion: controlplane.ProtocolVersion,
		Code:            code,
	})
	response, err := c.managerClient().ValidateOpenToken(ctx, request)
	if err != nil {
		return gatewayOpenTokenDecision{}, fmt.Errorf("validate open token rpc: %w", err)
	}
	if allowed := response.Msg.GetAllowed(); allowed != nil {
		return gatewayOpenTokenDecision{
			allowed: true,
			binding: gatewayOpenTokenBinding{
				userID:                allowed.GetUserId(),
				codespaceUUID:         allowed.GetRuntimeUuid(),
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
		ProtocolVersion: controlplane.ProtocolVersion,
		RuntimeUuid:     codespaceUUID,
		EndpointId:      endpointID,
	})
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
		ProtocolVersion: controlplane.ProtocolVersion,
		RuntimeUuid:     codespaceUUID,
		PublicKey:       append([]byte(nil), publicKey...),
	})
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

func (c *gatewayControlPlane) reportRuntimeMetadata(ctx context.Context, codespaceUUID string, metadata *codespacev1.RuntimeMetadata, metadataGeneration int64) error {
	request := connect.NewRequest(&codespacev1.ReportRuntimeMetadataRequest{
		ProtocolVersion:    controlplane.ProtocolVersion,
		RuntimeUuid:        codespaceUUID,
		MetadataGeneration: metadataGeneration,
		Metadata:           metadata,
	})
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
		ProtocolVersion: controlplane.ProtocolVersion,
		Session: &codespacev1.RevalidateGatewaySessionRequest_Endpoint{
			Endpoint: &codespacev1.EndpointSessionBinding{
				UserId:      userID,
				RuntimeUuid: codespaceUUID,
				EndpointId:  endpointID,
			},
		},
	})
}

func (c *gatewayControlPlane) revalidateSSHSession(ctx context.Context, userID int64, codespaceUUID string) (gatewayAccessDecision, error) {
	return c.revalidateGatewaySession(ctx, &codespacev1.RevalidateGatewaySessionRequest{
		ProtocolVersion: controlplane.ProtocolVersion,
		Session: &codespacev1.RevalidateGatewaySessionRequest_Ssh{
			Ssh: &codespacev1.SSHSessionBinding{
				UserId:      userID,
				RuntimeUuid: codespaceUUID,
			},
		},
	})
}

func (c *gatewayControlPlane) revalidateGatewaySession(ctx context.Context, payload *codespacev1.RevalidateGatewaySessionRequest) (gatewayAccessDecision, error) {
	request := connect.NewRequest(payload)
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

const managerServicePath = "/api/codespace"

func managerServiceBaseURL(giteaURL string) string {
	return strings.TrimRight(strings.TrimSpace(giteaURL), "/") + managerServicePath
}

func normalizeGiteaURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("gitea_url must be an absolute URL")
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("gitea_url scheme must be http or https")
	}
	return value, nil
}
