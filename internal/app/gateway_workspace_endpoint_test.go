// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"strings"
	"testing"

	"gitea.dev/codespace/internal/devcontainer"
	"gitea.dev/codespace/internal/provisioner"
)

func newTestWorkspaceEndpointRoutes(t *testing.T, codespaceUUID, upstreamURL string) *gatewayRouteStore {
	t.Helper()
	backend := newTestWorkspaceCommandBackend("")
	backend.tcpAddress = strings.TrimPrefix(upstreamURL, "http://")
	routes := newGatewayRouteStore()
	routes.SetTCPBackend(backend)
	if err := routes.Put(gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     devcontainer.WorkspaceEndpointID,
		label:          devcontainer.WorkspaceEndpointLabel,
		upstreamScheme: "http",
		instanceName:   "runtime-1",
		upstreamPort:   provisioner.WorkspaceIDEPort,
	}); err != nil {
		t.Fatalf("put workspace endpoint: %v", err)
	}
	return routes
}
