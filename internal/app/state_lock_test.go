// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

func TestStateDirLockRejectsSecondHolder(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	first, err := acquireStateDirLock(stateDir)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer first.Close()

	if _, err := acquireStateDirLock(stateDir); err == nil {
		t.Fatalf("expected second lock to fail")
	}
}

func TestRunWithConfigStateDirLockFailsBeforeRPC(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := SaveManagerCredentials(stateDir, ManagerCredentials{
		ManagerID:     42,
		ManagerSecret: "manager-secret",
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	lock, err := acquireStateDirLock(stateDir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Close()

	service := &lockTestManagerService{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	var output bytes.Buffer
	config := DefaultConfig()
	config.Server.ListenAddr = "127.0.0.1:0"
	config.Gitea.URL = server.URL
	config.Manager.StateDir = stateDir
	config.Manager.HTTPTimeout = Duration(100 * time.Millisecond)
	err = RunWithConfig(&output, config)
	if err == nil {
		t.Fatalf("expected locked state dir error")
	}
	if !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("unexpected error: %v", err)
	}
	if service.calls.Load() != 0 {
		t.Fatalf("manager service calls = %d", service.calls.Load())
	}
}

func TestRunWithConfigMissingRootStateFailsBeforeRPC(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := SaveManagerCredentials(stateDir, ManagerCredentials{
		ManagerID:     42,
		ManagerSecret: "manager-secret",
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	service := &lockTestManagerService{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	var output bytes.Buffer
	config := DefaultConfig()
	config.Server.ListenAddr = "127.0.0.1:0"
	config.Gitea.URL = server.URL
	config.Manager.StateDir = stateDir
	config.Manager.HTTPTimeout = Duration(100 * time.Millisecond)
	err := RunWithConfig(&output, config)
	if err == nil {
		t.Fatalf("expected missing root state error")
	}
	if !strings.Contains(err.Error(), "manager.json") {
		t.Fatalf("unexpected error: %v", err)
	}
	if service.calls.Load() != 0 {
		t.Fatalf("manager service calls = %d", service.calls.Load())
	}
}

func TestRunWithConfigListenerBindFailsBeforeRPC(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := SaveManagerCredentials(stateDir, ManagerCredentials{
		ManagerID:     42,
		ManagerSecret: "manager-secret",
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	if err := SaveManagerRootState(stateDir, ManagerRootState{
		ManagerID: 42,
	}); err != nil {
		t.Fatalf("save root state: %v", err)
	}

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen occupied address: %v", err)
	}
	defer occupied.Close()

	service := &lockTestManagerService{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	var output bytes.Buffer
	config := DefaultConfig()
	config.Server.ListenAddr = "127.0.0.1:0"
	config.Server.RuntimeAPIListenAddr = "127.0.0.1:0"
	config.Server.GatewayListenAddr = occupied.Addr().String()
	config.Server.GatewaySSHListenAddr = "127.0.0.1:0"
	config.Gitea.URL = server.URL
	config.Manager.StateDir = stateDir
	config.Manager.HTTPTimeout = Duration(100 * time.Millisecond)
	err = RunWithConfig(&output, config)
	if err == nil {
		t.Fatalf("expected listener bind error")
	}
	if !strings.Contains(err.Error(), "listen gateway http") {
		t.Fatalf("unexpected error: %v", err)
	}
	if service.calls.Load() != 0 {
		t.Fatalf("manager service calls = %d", service.calls.Load())
	}
}

type lockTestManagerService struct {
	codespacev1connect.UnimplementedManagerServiceHandler

	calls atomic.Int64
}

func (s *lockTestManagerService) DeclareManager(
	_ context.Context,
	_ *connect.Request[codespacev1.DeclareManagerRequest],
) (*connect.Response[codespacev1.DeclareManagerResponse], error) {
	s.calls.Add(1)
	return connect.NewResponse(&codespacev1.DeclareManagerResponse{}), nil
}
