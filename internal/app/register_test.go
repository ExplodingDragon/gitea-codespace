// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

func TestRegisterWritesManagerCredentials(t *testing.T) {
	t.Parallel()

	service := &registerService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()

	workdir := t.TempDir()
	input := bytes.NewBufferString(server.URL + "\nregistration-token\n")
	var output bytes.Buffer
	if err := Register(&output, input, filepath.Join(workdir, "existing.json")); err != nil {
		t.Fatalf("register: %v", err)
	}

	configPath := filepath.Join(workdir, defaultRegisterConfigPath)
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("generated config exists or stat failed: %v", err)
	}
	stateDir := filepath.Join(workdir, "codespace-state")
	identity, err := LoadManagerIdentity(stateDir)
	if err != nil {
		t.Fatalf("load manager identity: %v", err)
	}
	if identity.GiteaURL != server.URL {
		t.Fatalf("gitea url = %q", identity.GiteaURL)
	}
	if identity.ManagerID != 42 {
		t.Fatalf("identity manager id = %d", identity.ManagerID)
	}
	credentials, err := LoadManagerCredentials(stateDir)
	if err != nil {
		t.Fatalf("load manager credentials: %v", err)
	}
	if credentials.ManagerSecret != "manager-secret" {
		t.Fatalf("manager secret = %q", credentials.ManagerSecret)
	}
	rootState, err := LoadManagerRootState(stateDir, identity)
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

func TestRegisterRejectsInvalidGatewayConfig(t *testing.T) {
	t.Parallel()

	service := &registerService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "codespace.yaml")
	if err := os.WriteFile(configPath, []byte(`
gateway:
  http:
    public_url: "http://127.0.0.1:18081"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	input := bytes.NewBufferString(server.URL + "\nregistration-token\n")
	var output bytes.Buffer
	err := Register(&output, input, configPath)
	if err == nil {
		t.Fatalf("expected register config validation error")
	}
	if !strings.Contains(err.Error(), "gateway.http.public_url") {
		t.Fatalf("register error = %v", err)
	}
	if service.registerCalls != 0 {
		t.Fatalf("register calls = %d", service.registerCalls)
	}
}

func TestManagerServiceBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		giteaURL string
		want     string
	}{
		{
			name:     "root",
			giteaURL: "http://127.0.0.1:3000/",
			want:     "http://127.0.0.1:3000/api/codespace",
		},
		{
			name:     "sub url",
			giteaURL: "https://gitea.example.com/git/",
			want:     "https://gitea.example.com/git/api/codespace",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := managerServiceBaseURL(test.giteaURL); got != test.want {
				t.Fatalf("manager service base url = %q, want %q", got, test.want)
			}
		})
	}
}

type registerService struct {
	codespacev1connect.UnimplementedManagerServiceHandler

	registrationToken string
	protocolVersion   int32
	registerCalls     int
}

func (s *registerService) RegisterManager(
	_ context.Context,
	req *connect.Request[codespacev1.RegisterManagerRequest],
) (*connect.Response[codespacev1.RegisterManagerResponse], error) {
	s.registerCalls++
	s.registrationToken = req.Msg.GetRegistrationToken()
	s.protocolVersion = req.Msg.GetProtocolVersion()
	return connect.NewResponse(&codespacev1.RegisterManagerResponse{
		ManagerId:     42,
		ManagerSecret: "manager-secret",
	}), nil
}
