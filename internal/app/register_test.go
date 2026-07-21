// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

func TestRegisterWritesManagerCredentials(t *testing.T) {
	t.Parallel()

	service := &registerService{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	workdir := t.TempDir()
	input := bytes.NewBufferString(server.URL + "\nregistration-token\nregistered-manager\n")
	var output bytes.Buffer
	if err := Register(&output, input, filepath.Join(workdir, "existing.json")); err != nil {
		t.Fatalf("register: %v", err)
	}

	configPath := filepath.Join(workdir, defaultRegisterConfigPath)
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("stat generated config: %v", err)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	if config.Gitea.URL != server.URL {
		t.Fatalf("gitea url = %q", config.Gitea.URL)
	}
	if config.Manager.Name != "registered-manager" {
		t.Fatalf("manager name = %q", config.Manager.Name)
	}
	credentials, err := LoadManagerCredentials(config.Manager.StateDir)
	if err != nil {
		t.Fatalf("load manager credentials: %v", err)
	}
	if credentials.ManagerID != 42 {
		t.Fatalf("manager id = %d", credentials.ManagerID)
	}
	if credentials.ManagerSecret != "manager-secret" {
		t.Fatalf("manager secret = %q", credentials.ManagerSecret)
	}
	rootState, err := LoadManagerRootState(config.Manager.StateDir, credentials)
	if err != nil {
		t.Fatalf("load manager root state: %v", err)
	}
	if rootState.ManagerID != 42 {
		t.Fatalf("root state manager id = %d", rootState.ManagerID)
	}
	if rootState.InventoryGeneration != 0 {
		t.Fatalf("root state inventory generation = %d", rootState.InventoryGeneration)
	}
	if service.registrationToken != "registration-token" {
		t.Fatalf("registration token = %q", service.registrationToken)
	}
	if service.protocolVersion != 1 {
		t.Fatalf("protocol version = %d", service.protocolVersion)
	}
}

type registerService struct {
	codespacev1connect.UnimplementedManagerServiceHandler

	registrationToken string
	protocolVersion   int32
}

func (s *registerService) RegisterManager(
	_ context.Context,
	req *connect.Request[codespacev1.RegisterManagerRequest],
) (*connect.Response[codespacev1.RegisterManagerResponse], error) {
	s.registrationToken = req.Msg.GetRegistrationToken()
	s.protocolVersion = req.Msg.GetProtocolVersion()
	return connect.NewResponse(&codespacev1.RegisterManagerResponse{
		ManagerId:     42,
		ManagerSecret: "manager-secret",
	}), nil
}
