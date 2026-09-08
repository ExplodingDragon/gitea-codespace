// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

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
