// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"strings"
	"testing"

	"gitea.dev/codespace/internal/manager"
)

func newTestGatewayWorkspaceIDERoutes(t *testing.T, codespaceUUID, upstreamURL string) *gatewayRouteStore {
	t.Helper()
	store := NewCodespaceStateStore(t.TempDir())
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 1,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       100,
			LastUpdateUnix:    100,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}
	saveGatewayWorkspaceIdentityForTest(t, store, codespaceUUID)
	backend := newTestWorkspaceCommandBackend("")
	backend.tcpAddress = strings.TrimPrefix(upstreamURL, "http://")
	routes := newGatewayRouteStore()
	routes.SetWorkspaceIDE(newGatewayWorkspaceIDE(store, backend))
	return routes
}
