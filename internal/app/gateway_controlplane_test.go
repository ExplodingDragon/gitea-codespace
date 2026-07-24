// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

func TestGatewayControlPlaneValidateOpenTokenAllowed(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:                42,
					CodespaceUuid:         "11111111-1111-4111-8111-111111111111",
					EndpointId:            "workspace",
					InteractionGeneration: 7,
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()

	decision, err := controlPlane.validateOpenToken(context.Background(), "open-code")
	if err != nil {
		t.Fatalf("validate open token: %v", err)
	}
	if !decision.allowed ||
		decision.binding.userID != 42 ||
		decision.binding.codespaceUUID != "11111111-1111-4111-8111-111111111111" ||
		decision.binding.endpointID != "workspace" ||
		decision.binding.interactionGeneration != 7 {
		t.Fatalf("open token decision = %#v", decision)
	}
	if service.openTokenRequest.GetProtocolVersion() != 1 || service.openTokenRequest.GetCode() != "open-code" {
		t.Fatalf("open token request = %#v", service.openTokenRequest)
	}
	assertGatewayManagerAuth(t, service)
}

func TestGatewayControlPlaneValidatePublicEndpointDenied(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Denied{
				Denied: &codespacev1.FailureDetail{Category: "state_unavailable"},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()

	decision, err := controlPlane.validatePublicEndpoint(context.Background(), "codespace-uuid", "web")
	if err != nil {
		t.Fatalf("validate public endpoint: %v", err)
	}
	if decision.allowed || decision.deniedCategory != "state_unavailable" {
		t.Fatalf("public endpoint decision = %#v", decision)
	}
	if service.publicEndpointRequest.GetProtocolVersion() != 1 ||
		service.publicEndpointRequest.GetCodespaceUuid() != "codespace-uuid" ||
		service.publicEndpointRequest.GetEndpointId() != "web" {
		t.Fatalf("public endpoint request = %#v", service.publicEndpointRequest)
	}
	assertGatewayManagerAuth(t, service)
}

func TestGatewayControlPlaneVerifySSHPublicKeyAllowed(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		sshResponse: &codespacev1.VerifySSHPublicKeyResponse{
			Outcome: &codespacev1.VerifySSHPublicKeyResponse_Allowed{
				Allowed: &codespacev1.SSHAuthBinding{
					UserId:                42,
					InteractionGeneration: 8,
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()

	publicKey := []byte("ssh-wire-key")
	decision, err := controlPlane.verifySSHPublicKey(context.Background(), "codespace-uuid", publicKey)
	if err != nil {
		t.Fatalf("verify ssh public key: %v", err)
	}
	publicKey[0] = 'X'
	if !decision.allowed || decision.userID != 42 || decision.interactionGeneration != 8 {
		t.Fatalf("ssh decision = %#v", decision)
	}
	if string(service.sshRequest.GetPublicKey()) != "ssh-wire-key" {
		t.Fatalf("ssh public key request = %q", service.sshRequest.GetPublicKey())
	}
	assertGatewayManagerAuth(t, service)
}

func TestGatewayControlPlaneEnsureCodespaceGitSSHKey(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		ensureGitSSHKeyResponse: &codespacev1.EnsureCodespaceGitSSHKeyResponse{
			KnownHostsLines: []string{"gitea.example.com ssh-ed25519 AAAA"},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()

	publicKey := []byte("ssh-wire-key")
	lines, err := controlPlane.ensureCodespaceGitSSHKey(context.Background(), "codespace-uuid", publicKey)
	if err != nil {
		t.Fatalf("ensure git ssh key: %v", err)
	}
	publicKey[0] = 'X'
	if len(lines) != 1 || lines[0] != "gitea.example.com ssh-ed25519 AAAA" {
		t.Fatalf("known hosts lines = %#v", lines)
	}
	if service.ensureGitSSHKeyRequest.GetProtocolVersion() != 1 ||
		service.ensureGitSSHKeyRequest.GetCodespaceUuid() != "codespace-uuid" ||
		string(service.ensureGitSSHKeyRequest.GetPublicKey()) != "ssh-wire-key" {
		t.Fatalf("ensure git ssh key request = %#v", service.ensureGitSSHKeyRequest)
	}
	assertGatewayManagerAuth(t, service)
}

func TestGatewayControlPlaneReportRuntimeMetadata(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()

	if err := controlPlane.reportRuntimeMetadata(context.Background(), "codespace-uuid", `{"endpoints":[]}`, 3); err != nil {
		t.Fatalf("report runtime metadata: %v", err)
	}
	if service.metadataRequest.GetProtocolVersion() != 1 ||
		service.metadataRequest.GetCodespaceUuid() != "codespace-uuid" ||
		service.metadataRequest.GetMetadataJson() != `{"endpoints":[]}` ||
		service.metadataRequest.GetMetadataGeneration() != 3 {
		t.Fatalf("metadata request = %#v", service.metadataRequest)
	}
	assertGatewayManagerAuth(t, service)
}

func TestGatewayControlPlaneRevalidateSSHSessionAllowed(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()

	decision, err := controlPlane.revalidateSSHSession(context.Background(), 42, "codespace-uuid")
	if err != nil {
		t.Fatalf("revalidate ssh session: %v", err)
	}
	if !decision.allowed || decision.deniedCategory != "" {
		t.Fatalf("revalidate decision = %#v", decision)
	}
	if service.revalidateRequest.GetProtocolVersion() != 1 ||
		service.revalidateRequest.GetSsh().GetUserId() != 42 ||
		service.revalidateRequest.GetSsh().GetCodespaceUuid() != "codespace-uuid" {
		t.Fatalf("revalidate request = %#v", service.revalidateRequest)
	}
	assertGatewayManagerAuth(t, service)
}

func TestGatewayControlPlaneMissingOutcomeFails(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()

	if _, err := controlPlane.validatePublicEndpoint(context.Background(), "codespace-uuid", "web"); err == nil {
		t.Fatalf("expected missing outcome error")
	}
}

type gatewayManagerService struct {
	codespacev1connect.UnimplementedManagerServiceHandler

	mu                      sync.Mutex
	managerID               string
	managerSecret           string
	openTokenRequest        *codespacev1.ValidateOpenTokenRequest
	publicEndpointRequest   *codespacev1.ValidatePublicEndpointRequest
	sshRequest              *codespacev1.VerifySSHPublicKeyRequest
	ensureGitSSHKeyRequest  *codespacev1.EnsureCodespaceGitSSHKeyRequest
	metadataRequest         *codespacev1.ReportRuntimeMetadataRequest
	revalidateRequest       *codespacev1.RevalidateGatewaySessionRequest
	publicEndpointCalls     int
	openTokenResponse       *codespacev1.ValidateOpenTokenResponse
	openTokenCalls          int
	openTokenStarted        chan struct{}
	openTokenRelease        chan struct{}
	publicEndpointResponse  *codespacev1.ValidatePublicEndpointResponse
	publicEndpointStarted   chan struct{}
	publicEndpointRelease   chan struct{}
	sshResponse             *codespacev1.VerifySSHPublicKeyResponse
	ensureGitSSHKeyResponse *codespacev1.EnsureCodespaceGitSSHKeyResponse
	metadataErr             error
	metadataResponse        *codespacev1.ReportRuntimeMetadataResponse
	metadataCalls           int
	metadataStarted         chan struct{}
	metadataRelease         chan struct{}
	revalidateResponse      *codespacev1.RevalidateGatewaySessionResponse
	revalidateCalls         int
	revalidateStarted       chan struct{}
	revalidateRelease       chan struct{}
}

func (s *gatewayManagerService) ValidateOpenToken(
	_ context.Context,
	req *connect.Request[codespacev1.ValidateOpenTokenRequest],
) (*connect.Response[codespacev1.ValidateOpenTokenResponse], error) {
	s.captureAuth(req.Header())
	request := *req.Msg
	s.mu.Lock()
	s.openTokenRequest = &request
	s.openTokenCalls++
	response := s.openTokenResponse
	started := s.openTokenStarted
	release := s.openTokenRelease
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if response != nil {
		return connect.NewResponse(response), nil
	}
	return connect.NewResponse(&codespacev1.ValidateOpenTokenResponse{}), nil
}

func (s *gatewayManagerService) ValidatePublicEndpoint(
	_ context.Context,
	req *connect.Request[codespacev1.ValidatePublicEndpointRequest],
) (*connect.Response[codespacev1.ValidatePublicEndpointResponse], error) {
	s.captureAuth(req.Header())
	request := *req.Msg
	s.mu.Lock()
	s.publicEndpointRequest = &request
	s.publicEndpointCalls++
	response := s.publicEndpointResponse
	started := s.publicEndpointStarted
	release := s.publicEndpointRelease
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if response != nil {
		return connect.NewResponse(response), nil
	}
	return connect.NewResponse(&codespacev1.ValidatePublicEndpointResponse{}), nil
}

func (s *gatewayManagerService) VerifySSHPublicKey(
	_ context.Context,
	req *connect.Request[codespacev1.VerifySSHPublicKeyRequest],
) (*connect.Response[codespacev1.VerifySSHPublicKeyResponse], error) {
	s.captureAuth(req.Header())
	request := *req.Msg
	request.PublicKey = append([]byte(nil), req.Msg.GetPublicKey()...)
	s.mu.Lock()
	s.sshRequest = &request
	response := s.sshResponse
	s.mu.Unlock()
	if response != nil {
		return connect.NewResponse(response), nil
	}
	return connect.NewResponse(&codespacev1.VerifySSHPublicKeyResponse{}), nil
}

func (s *gatewayManagerService) EnsureCodespaceGitSSHKey(
	_ context.Context,
	req *connect.Request[codespacev1.EnsureCodespaceGitSSHKeyRequest],
) (*connect.Response[codespacev1.EnsureCodespaceGitSSHKeyResponse], error) {
	s.captureAuth(req.Header())
	request := *req.Msg
	request.PublicKey = append([]byte(nil), req.Msg.GetPublicKey()...)
	s.mu.Lock()
	s.ensureGitSSHKeyRequest = &request
	response := s.ensureGitSSHKeyResponse
	s.mu.Unlock()
	if response != nil {
		return connect.NewResponse(response), nil
	}
	return connect.NewResponse(&codespacev1.EnsureCodespaceGitSSHKeyResponse{}), nil
}

func (s *gatewayManagerService) ReportRuntimeMetadata(
	_ context.Context,
	req *connect.Request[codespacev1.ReportRuntimeMetadataRequest],
) (*connect.Response[codespacev1.ReportRuntimeMetadataResponse], error) {
	s.captureAuth(req.Header())
	request := *req.Msg
	s.mu.Lock()
	s.metadataRequest = &request
	s.metadataCalls++
	err := s.metadataErr
	response := s.metadataResponse
	started := s.metadataStarted
	release := s.metadataRelease
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return nil, err
	}
	if response != nil {
		return connect.NewResponse(response), nil
	}
	return connect.NewResponse(&codespacev1.ReportRuntimeMetadataResponse{}), nil
}

func (s *gatewayManagerService) RevalidateGatewaySession(
	_ context.Context,
	req *connect.Request[codespacev1.RevalidateGatewaySessionRequest],
) (*connect.Response[codespacev1.RevalidateGatewaySessionResponse], error) {
	s.captureAuth(req.Header())
	request := *req.Msg
	s.mu.Lock()
	s.revalidateRequest = &request
	s.revalidateCalls++
	response := s.revalidateResponse
	started := s.revalidateStarted
	release := s.revalidateRelease
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if response != nil {
		return connect.NewResponse(response), nil
	}
	return connect.NewResponse(&codespacev1.RevalidateGatewaySessionResponse{}), nil
}

func (s *gatewayManagerService) captureAuth(header http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.managerID = header.Get(gatewayManagerIDHeader)
	s.managerSecret = header.Get(gatewayManagerSecretHeader)
}

func (s *gatewayManagerService) publicEndpointCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.publicEndpointCalls
}

func (s *gatewayManagerService) openTokenCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.openTokenCalls
}

func (s *gatewayManagerService) revalidateCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.revalidateCalls
}

func (s *gatewayManagerService) setPublicEndpointResponse(response *codespacev1.ValidatePublicEndpointResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.publicEndpointResponse = response
}

func (s *gatewayManagerService) setRevalidateResponse(response *codespacev1.RevalidateGatewaySessionResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.revalidateResponse = response
}

func newTestGatewayControlPlane(t *testing.T, service *gatewayManagerService) (*gatewayControlPlane, func()) {
	t.Helper()

	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	controlPlane := newGatewayControlPlane(server.URL, 7, "manager-secret", server.Client())
	return controlPlane, server.Close
}

func assertGatewayManagerAuth(t *testing.T, service *gatewayManagerService) {
	t.Helper()

	service.mu.Lock()
	defer service.mu.Unlock()

	if service.managerID != "7" || service.managerSecret != "manager-secret" {
		t.Fatalf("manager auth headers = %q/%q", service.managerID, service.managerSecret)
	}
}
