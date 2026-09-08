// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gitea.dev/codespace/internal/manager"
)

func TestRuntimeEndpointApplierUpdatesRoutesAndNotifiesOnChange(t *testing.T) {

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	state := newTestCodespaceStateStore(t, filepath.Join(t.TempDir(), "state"))
	notifier := &runtimeEndpointNotifierForTest{}
	applier := &runtimeEndpointApplier{state: state, publisher: notifier}
	endpointRoutes := completeEndpointRoutesForTest(codespaceUUID, manager.RuntimeEndpointRoute{
		CodespaceUUID: codespaceUUID,
		EndpointID:    "web",
		Label:         "Web",
		InstanceName:  "runtime-1",
		UpstreamPort:  3000,
		Public:        true,
	})

	if err := applier.ApplyRuntimeEndpointRoutes(context.Background(), codespaceUUID, endpointRoutes); err != nil {
		t.Fatalf("apply endpoint routes: %v", err)
	}
	if notifier.calls != 1 || notifier.codespaceUUID != codespaceUUID {
		t.Fatalf("metadata notifications = %d uuid=%q", notifier.calls, notifier.codespaceUUID)
	}
	routes, err := state.LoadGatewayRoutesForRuntime(codespaceUUID)
	if err != nil || len(routes) != 2 {
		t.Fatalf("saved routes = %#v, error = %v", routes, err)
	}

	if err := applier.ApplyRuntimeEndpointRoutes(context.Background(), codespaceUUID, endpointRoutes); err != nil {
		t.Fatalf("reapply endpoint routes: %v", err)
	}
	if notifier.calls != 1 {
		t.Fatalf("same endpoint routes triggered metadata notification count %d", notifier.calls)
	}
}

type runtimeEndpointNotifierForTest struct {
	codespaceUUID string
	calls         int
}

func (n *runtimeEndpointNotifierForTest) NotifyRuntimeMetadata(codespaceUUID string) {
	n.codespaceUUID = codespaceUUID
	n.calls++
}

type canceledGatewaySnapshotStore struct{ managerInfrastructureStore }

func (canceledGatewaySnapshotStore) DeleteGatewayRuntime(ctx context.Context, _ string) error {
	return ctx.Err()
}

func TestRuntimeEndpointApplierPropagatesCancellation(t *testing.T) {
	state := newTestCodespaceStateStore(t, t.TempDir())
	applier := &runtimeEndpointApplier{state: state, snapshots: &gatewayRuntimeSnapshotPublisher{store: canceledGatewaySnapshotStore{}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := applier.ApplyRuntimeEndpointRoutes(ctx, "11111111-1111-4111-8111-111111111111", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("apply canceled routes: %v", err)
	}
}
