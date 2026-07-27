// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

func TestNewManagerServiceClientAddsAuthentication(t *testing.T) {
	t.Parallel()

	service := &authenticationService{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewManagerServiceClient(server.Client(), server.URL, 42, "manager-secret", 1024)
	_, err := client.DeclareManager(t.Context(), connect.NewRequest(&codespacev1.DeclareManagerRequest{
		ProtocolVersion: ProtocolVersion,
	}))
	if err != nil {
		t.Fatalf("declare manager: %v", err)
	}
	if service.managerID != "42" || service.managerSecret != "manager-secret" {
		t.Fatalf("manager authentication = (%q, %q)", service.managerID, service.managerSecret)
	}
}

func TestCheckMessageSize(t *testing.T) {
	t.Parallel()

	message := &codespacev1.RegisterManagerRequest{RegistrationToken: "token"}
	if err := CheckMessageSize(message, 0); err != nil {
		t.Fatalf("unlimited message size: %v", err)
	}
	if err := CheckMessageSize(message, 1); err == nil {
		t.Fatal("expected message size error")
	}
}

type authenticationService struct {
	codespacev1connect.UnimplementedManagerServiceHandler
	managerID     string
	managerSecret string
}

func (s *authenticationService) DeclareManager(
	_ context.Context,
	request *connect.Request[codespacev1.DeclareManagerRequest],
) (*connect.Response[codespacev1.DeclareManagerResponse], error) {
	s.managerID = request.Header().Get(ManagerIDHeader)
	s.managerSecret = request.Header().Get(ManagerSecretHeader)
	return connect.NewResponse(&codespacev1.DeclareManagerResponse{}), nil
}
