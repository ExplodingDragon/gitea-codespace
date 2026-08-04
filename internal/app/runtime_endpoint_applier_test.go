// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"path/filepath"
	"testing"

	"gitea.dev/codespace/internal/manager"
)

func TestRuntimeEndpointApplierUpdatesRoutesAndNotifiesOnChange(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	state := NewCodespaceStateStore(filepath.Join(t.TempDir(), "state"))
	routes := newGatewayRouteStore()
	notifier := &runtimeEndpointNotifierForTest{}
	applier := newRuntimeEndpointApplier(state, routes, notifier)
	endpointRoutes := completeEndpointRoutesForTest(codespaceUUID, manager.RuntimeEndpointRoute{
		CodespaceUUID: codespaceUUID,
		EndpointID:    "web",
		Label:         "Web",
		InstanceName:  "runtime-1",
		UpstreamPort:  3000,
		Public:        true,
	})

	if err := applier.ApplyRuntimeEndpointRoutes(codespaceUUID, endpointRoutes); err != nil {
		t.Fatalf("apply endpoint routes: %v", err)
	}
	if notifier.calls != 1 || notifier.codespaceUUID != codespaceUUID {
		t.Fatalf("metadata notifications = %d uuid=%q", notifier.calls, notifier.codespaceUUID)
	}
	route, ok := routes.Get(codespaceUUID, "web")
	if !ok || !route.public || route.instanceName != "runtime-1" || route.upstreamPort != 3000 {
		t.Fatalf("gateway route ok=%v route=%#v", ok, route)
	}

	if err := applier.ApplyRuntimeEndpointRoutes(codespaceUUID, endpointRoutes); err != nil {
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
