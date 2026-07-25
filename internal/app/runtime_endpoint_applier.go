// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"fmt"

	"gitea.dev/codespace/internal/manager"
)

type runtimeEndpointApplier struct {
	state     *CodespaceStateStore
	routes    *gatewayRouteStore
	publisher runtimeMetadataNotifier
}

func newRuntimeEndpointApplier(state *CodespaceStateStore, routes *gatewayRouteStore, publisher runtimeMetadataNotifier) *runtimeEndpointApplier {
	return &runtimeEndpointApplier{
		state:     state,
		routes:    routes,
		publisher: publisher,
	}
}

func (a *runtimeEndpointApplier) ApplyRuntimeEndpointRoutes(codespaceUUID string, routes []manager.RuntimeEndpointRoute) error {
	if a == nil || a.state == nil {
		return fmt.Errorf("runtime endpoint applier is not ready")
	}
	changed, err := a.state.SaveRuntimeEndpointRoutes(codespaceUUID, routes)
	if err != nil {
		return fmt.Errorf("save runtime endpoint routes: %w", err)
	}
	if a.routes != nil {
		if err := a.routes.ReplaceRuntimeEndpointRoutes(codespaceUUID, routes); err != nil {
			return fmt.Errorf("replace gateway endpoint routes: %w", err)
		}
	}
	if changed && a.publisher != nil {
		a.publisher.NotifyRuntimeMetadata(codespaceUUID)
	}
	return nil
}
