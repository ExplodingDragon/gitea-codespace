// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"sync"

	"gitea.dev/codespace/internal/manager"
)

type runtimeEndpointApplier struct {
	state     *CodespaceStateStore
	publisher runtimeMetadataNotifier
	snapshots *gatewayRuntimeSnapshotPublisher
}

func (a *runtimeEndpointApplier) ApplyRuntimeEndpointRoutes(ctx context.Context, codespaceUUID string, routes []manager.RuntimeEndpointRoute) error {
	if a == nil || a.state == nil {
		return fmt.Errorf("runtime endpoint applier is not ready")
	}
	changed, err := a.state.SaveRuntimeEndpointRoutes(codespaceUUID, routes)
	if err != nil {
		return fmt.Errorf("save runtime endpoint routes: %w", err)
	}
	if a.snapshots != nil {
		var snapshotErr error
		if len(routes) == 0 {
			snapshotErr = a.snapshots.Delete(ctx, codespaceUUID)
		} else {
			snapshotErr = a.snapshots.Sync(ctx, codespaceUUID)
		}
		if snapshotErr != nil {
			return fmt.Errorf("save shared gateway runtime: %w", snapshotErr)
		}
	}
	if changed && a.publisher != nil {
		a.publisher.NotifyRuntimeMetadata(codespaceUUID)
	}
	return nil
}

type gatewayRuntimeSnapshotPublisher struct {
	// Serialize publication and deletion so a delayed read cannot republish a stopped runtime.
	mu     sync.Mutex
	siteID int64
	state  *CodespaceStateStore
	store  managerInfrastructureStore
}

func (p *gatewayRuntimeSnapshotPublisher) Sync(ctx context.Context, codespaceUUID string) error {
	if p == nil || p.store == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	target, ok, err := p.state.LoadGatewayWorkspaceTarget(codespaceUUID)
	if err != nil {
		return err
	}
	if !ok {
		return p.store.DeleteGatewayRuntime(ctx, codespaceUUID)
	}
	routes, err := p.state.LoadGatewayRoutesForRuntime(codespaceUUID)
	if err != nil {
		return err
	}
	snapshot := GatewayRuntimeSnapshot{
		RuntimeUUID: codespaceUUID, SiteID: p.siteID, InstanceName: target.instanceName, Workdir: target.workdir,
		UID: target.uid, GID: target.gid, ContainerID: target.containerID, ContainerUser: target.containerUser,
		ContainerWorkdir: target.containerWorkdir, EditorPort: target.editorPort,
	}
	for _, route := range routes {
		snapshot.Endpoints = append(snapshot.Endpoints, GatewayEndpointSnapshot{
			EndpointID: route.endpointID, Label: route.label, UpstreamPort: route.upstreamPort, Public: route.public,
		})
	}
	return p.store.SaveGatewayRuntime(ctx, snapshot)
}

func (p *gatewayRuntimeSnapshotPublisher) Delete(ctx context.Context, codespaceUUID string) error {
	if p == nil || p.store == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.store.DeleteGatewayRuntime(ctx, codespaceUUID)
}

func loadSharedGatewayRuntimes(ctx context.Context, store managerInfrastructureStore, routes *gatewayRouteStore) error {
	if store == nil {
		return nil
	}
	snapshots, err := store.ListGatewayRuntimes(ctx)
	if err != nil {
		return fmt.Errorf("list shared gateway runtimes: %w", err)
	}
	if err := routes.ReplaceSharedGatewayRuntimes(snapshots); err != nil {
		return fmt.Errorf("apply shared gateway runtimes: %w", err)
	}
	return nil
}
