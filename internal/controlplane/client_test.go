// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

func TestNewManagerServiceClientAddsAuthentication(t *testing.T) {
	t.Parallel()

	service := &authenticationService{}
	rpcServer := connect.NewServer()
	codespacev1connect.RegisterManagerServiceHandler(rpcServer, service)
	handler := http.NewServeMux()
	connecthttp.Mount(handler, rpcServer)
	path := "/" + codespacev1connect.ManagerServiceName + "/"

	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewManagerServiceClient(server.Client(), server.URL, 42, "manager-secret")
	response, err := client.DeclareManager(t.Context(), &codespacev1.DeclareManagerRequest{
		ProtocolVersion: ProtocolVersion,
		Version:         strings.Repeat("a", 2*1024*1024),
	})
	if err != nil {
		t.Fatalf("declare manager: %v", err)
	}
	if len(response.GetGiteaWebUrl()) != 2*1024*1024 {
		t.Fatal("control-plane response was truncated")
	}
	if service.managerID != "42" || service.managerSecret != "manager-secret" {
		t.Fatalf("manager authentication = (%q, %q)", service.managerID, service.managerSecret)
	}
}

type authenticationService struct {
	codespacev1connect.UnimplementedManagerServiceHandler
	managerID     string
	managerSecret string
}

func (s *authenticationService) DeclareManager(
	ctx context.Context,
	request *codespacev1.DeclareManagerRequest,
) (*codespacev1.DeclareManagerResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	s.managerID = info.RequestHeader().Get(ManagerIDHeader)
	s.managerSecret = info.RequestHeader().Get(ManagerSecretHeader)
	return &codespacev1.DeclareManagerResponse{GiteaWebUrl: request.GetVersion()}, nil
}
