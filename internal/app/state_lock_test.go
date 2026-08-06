// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"net"
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
	defer func() { _ = first.Close() }()

	if _, err := acquireStateDirLock(stateDir); err == nil {
		t.Fatalf("expected second lock to fail")
	}
}

func TestRunWithConfigStateDirLockFailsBeforeRPC(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	lock, err := acquireStateDirLock(stateDir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer func() { _ = lock.Close() }()
	managerState := saveManagerStateForTest(t, stateDir, "https://gitea.example.com", 42)

	service := &lockTestManagerService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()

	var output bytes.Buffer
	config := DefaultConfig()
	config.Gateway.HTTP.Listen = "127.0.0.1:0"
	config.Node.StateDir = stateDir
	config.Node.HTTPTimeout = Duration(100 * time.Millisecond)
	err = RunWithInfrastructureConfig(&output, InfrastructureRuntimeConfig{Config: config, ManagerState: managerState})
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

func TestRunWithConfigMissingManagerIdentityFailsBeforeRPC(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	service := &lockTestManagerService{}
	server := newLockTestManagerServer(t, service)
	defer server.Close()

	var output bytes.Buffer
	config := DefaultConfig()
	config.Gateway.HTTP.Listen = "127.0.0.1:0"
	config.Node.StateDir = stateDir
	config.Node.HTTPTimeout = Duration(100 * time.Millisecond)
	err := RunWithInfrastructureConfig(&output, InfrastructureRuntimeConfig{Config: config})
	if err == nil {
		t.Fatalf("expected missing manager identity error")
	}
	if !strings.Contains(err.Error(), "gitea_url") {
		t.Fatalf("unexpected error: %v", err)
	}
	if service.calls.Load() != 0 {
		t.Fatalf("manager service calls = %d", service.calls.Load())
	}
}

func TestRunWithConfigListenerBindFailsBeforeRPC(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen occupied address: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	service := &lockTestManagerService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()
	managerState := saveManagerStateForTest(t, stateDir, server.URL, 42)

	var output bytes.Buffer
	config := DefaultConfig()
	config.Gateway.HTTP.Listen = "127.0.0.1:0"
	config.Gateway.HTTP.Listen = occupied.Addr().String()
	config.Gateway.SSH.Listen = "127.0.0.1:0"
	config.Node.StateDir = stateDir
	config.Node.HTTPTimeout = Duration(100 * time.Millisecond)
	err = RunWithInfrastructureConfig(&output, InfrastructureRuntimeConfig{Config: config, ManagerState: managerState})
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
