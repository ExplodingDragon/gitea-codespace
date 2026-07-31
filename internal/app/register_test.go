// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

func TestRegisterWritesManagerState(t *testing.T) {
	t.Parallel()

	service := &registerService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()

	stateDir := filepath.Join(t.TempDir(), "state")
	configPath := writeRegisterConfig(t, stateDir)
	input := bytes.NewBufferString(server.URL + "/\nregistration-token\n")
	var output bytes.Buffer
	if err := Register(&output, input, configPath); err != nil {
		t.Fatalf("register: %v", err)
	}

	state, err := LoadManagerState(stateDir)
	if err != nil {
		t.Fatalf("load manager state: %v", err)
	}
	if state.GiteaURL != server.URL || state.ManagerID != 42 || state.ManagerSecret != "manager-secret" {
		t.Fatalf("manager state = %#v", state)
	}
	if state.InventoryGeneration != 0 {
		t.Fatalf("inventory generation = %d", state.InventoryGeneration)
	}
	info, err := os.Stat(filepath.Join(stateDir, managerStateFileName))
	if err != nil {
		t.Fatalf("stat manager state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manager state mode = %v", info.Mode().Perm())
	}
	if service.registrationToken != "registration-token" || service.protocolVersion != 1 || service.registerCalls != 1 {
		t.Fatalf("register service = %#v", service)
	}
}

func TestRegisterRejectsInvalidConfigurationBeforeRPC(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tests := []struct {
		name    string
		path    string
		content string
	}{
		{name: "missing", path: filepath.Join(dir, "missing.yaml")},
		{name: "malformed", path: filepath.Join(dir, "malformed.yaml"), content: "node: ["},
		{name: "unknown field", path: filepath.Join(dir, "unknown.yaml"), content: "unknown: true\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.content != "" {
				if err := os.WriteFile(test.path, []byte(test.content), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}
			var output bytes.Buffer
			err := Register(&output, bytes.NewBufferString("https://gitea.example.com\ntoken\n"), test.path)
			if err == nil || !strings.Contains(err.Error(), "load register config") {
				t.Fatalf("register error = %v", err)
			}
		})
	}
}

func TestLoadConfigForRegisterUsesDefaultsWhenNoConfigExists(t *testing.T) {
	t.Chdir(t.TempDir())

	config, err := LoadConfigForRegister("")
	if err != nil {
		t.Fatalf("load default register config: %v", err)
	}
	if config.Node.StateDir != "codespace-state" {
		t.Fatalf("state dir = %q", config.Node.StateDir)
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
	if err == nil || !strings.Contains(err.Error(), "gateway.http.public_url") {
		t.Fatalf("register error = %v", err)
	}
	if service.registerCalls != 0 {
		t.Fatalf("register calls = %d", service.registerCalls)
	}
}

func TestRegisterRejectsExistingStateBeforeRPC(t *testing.T) {
	t.Parallel()

	service := &registerService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()

	stateDir := filepath.Join(t.TempDir(), "state")
	saveManagerRegistrationForTest(t, stateDir, server.URL, 42)
	configPath := writeRegisterConfig(t, stateDir)
	var output bytes.Buffer
	err := Register(&output, bytes.NewBufferString(server.URL+"\nregistration-token\n"), configPath)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("register error = %v", err)
	}
	if !strings.Contains(err.Error(), server.URL) || !strings.Contains(err.Error(), "42") {
		t.Fatalf("register error lacks current identity: %v", err)
	}
	if service.registerCalls != 0 {
		t.Fatalf("register calls = %d", service.registerCalls)
	}
}

func TestRegisterRejectsDamagedStateBeforeRPC(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, managerStateFileName), []byte("{"), 0o600); err != nil {
		t.Fatalf("write damaged state: %v", err)
	}
	configPath := writeRegisterConfig(t, stateDir)
	var output bytes.Buffer
	err := Register(&output, bytes.NewBufferString("https://gitea.example.com\nregistration-token\n"), configPath)
	if err == nil || !strings.Contains(err.Error(), "already exists but is invalid") {
		t.Fatalf("register error = %v", err)
	}
}

func TestRegisterUsesStateDirectoryLock(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	lock, err := acquireStateDirLock(stateDir)
	if err != nil {
		t.Fatalf("acquire state lock: %v", err)
	}
	defer lock.Close()

	configPath := writeRegisterConfig(t, stateDir)
	var output bytes.Buffer
	err = Register(&output, bytes.NewBufferString("https://gitea.example.com\nregistration-token\n"), configPath)
	if err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("register error = %v", err)
	}
}

func TestManagerServiceBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		giteaURL string
		want     string
	}{
		{name: "root", giteaURL: "http://127.0.0.1:3000/", want: "http://127.0.0.1:3000/api/codespace"},
		{name: "sub url", giteaURL: "https://gitea.example.com/git/", want: "https://gitea.example.com/git/api/codespace"},
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

func writeRegisterConfig(t *testing.T, stateDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codespace.yaml")
	content := fmt.Sprintf(`
node:
  state_dir: %q
gateway:
  http:
    public_url: "http://gateway.example.com:18081"
  ssh:
    public_addr: "gateway.example.com:22"
`, stateDir)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write register config: %v", err)
	}
	return path
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
