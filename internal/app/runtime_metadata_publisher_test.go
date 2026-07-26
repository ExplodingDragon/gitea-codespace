// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gitea.dev/codespace/internal/manager"
)

func TestRuntimeMetadataPublisherSettingsWakeOnlyOnIntervalChange(t *testing.T) {
	t.Parallel()

	publisher := newRuntimeMetadataPublisher(nil, nil, nil, 0)
	settings := manager.ManagerServiceSettings{RuntimeMetadataRefreshInterval: time.Minute}
	if err := publisher.SaveManagerServiceSettings(settings); err != nil {
		t.Fatalf("save initial settings: %v", err)
	}
	if wakeLen := len(publisher.refreshWake); wakeLen != 1 {
		t.Fatalf("initial refresh wake len = %d, want 1", wakeLen)
	}
	<-publisher.refreshWake

	if err := publisher.SaveManagerServiceSettings(settings); err != nil {
		t.Fatalf("save unchanged settings: %v", err)
	}
	if wakeLen := len(publisher.refreshWake); wakeLen != 0 {
		t.Fatalf("unchanged refresh wake len = %d, want 0", wakeLen)
	}

	settings.RuntimeMetadataRefreshInterval = 2 * time.Minute
	if err := publisher.SaveManagerServiceSettings(settings); err != nil {
		t.Fatalf("save changed settings: %v", err)
	}
	if wakeLen := len(publisher.refreshWake); wakeLen != 1 {
		t.Fatalf("changed refresh wake len = %d, want 1", wakeLen)
	}
}

func TestRuntimeMetadataPublisherRefreshesWhileSettingsRepeat(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata snapshot: %v", err)
	}

	publisher := newRuntimeMetadataPublisher(store, controlPlane, nil, time.Millisecond)
	settings := manager.ManagerServiceSettings{RuntimeMetadataRefreshInterval: 40 * time.Millisecond}
	if err := publisher.SaveManagerServiceSettings(settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publisher.Run(ctx, []string{codespaceUUID})

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(180 * time.Millisecond)
		for {
			select {
			case <-deadline:
				return
			case <-ticker.C:
				_ = publisher.SaveManagerServiceSettings(settings)
			}
		}
	}()

	waitForMetadataCalls(t, service, 3)
	cancel()
	<-done
}

func waitForMetadataCalls(t *testing.T, service *gatewayManagerService, calls int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if service.metadataCallCount() >= calls {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("metadata rpc calls = %d, want at least %d", service.metadataCallCount(), calls)
}

func (s *gatewayManagerService) metadataCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.metadataCalls
}
