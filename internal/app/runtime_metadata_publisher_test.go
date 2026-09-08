// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
)

type shutdownResourceSampler struct {
	entered   chan struct{}
	release   chan struct{}
	state     *executionStateStore
	committed chan error
}

func (s *shutdownResourceSampler) RuntimeResourceUsage(ctx context.Context, _ string) (provisioner.RuntimeResourceUsage, error) {
	close(s.entered)
	<-ctx.Done()
	<-s.release
	s.committed <- s.state.save("last-observation", []byte("saved during shutdown"), 0)
	return provisioner.RuntimeResourceUsage{}, ctx.Err()
}

func TestRuntimeMetadataPublisherShutdownWaitsForLastStoreUse(t *testing.T) {
	store := newTestCodespaceStateStore(t, t.TempDir())
	const runtimeUUID = "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID: runtimeUUID, MetadataGeneration: 1,
		InstanceName: "cs-11111111111141118111", Workdir: "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{OperationRVersion: 1, Stage: manager.RuntimeBootStageReady, StartedUnix: 10, LastUpdateUnix: 11},
	}); err != nil {
		t.Fatal(err)
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, &gatewayManagerService{})
	defer closeServer()
	sampler := &shutdownResourceSampler{entered: make(chan struct{}), release: make(chan struct{}), state: store.records, committed: make(chan error, 1)}
	publisher := newRuntimeMetadataPublisher(store, controlPlane, sampler, time.Millisecond)
	publisher.Run(context.Background())
	released := false
	defer func() {
		if !released {
			close(sampler.release)
		}
		publisher.Close()
	}()
	if _, err := publisher.ActivateRuntimeMetadata(runtimeUUID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sampler.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not start")
	}
	done := make(chan struct{})
	go func() { publisher.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("publisher closed before its storage user returned")
	case <-time.After(50 * time.Millisecond):
	}
	close(sampler.release)
	released = true
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not finish shutdown")
	}
	if err := <-sampler.committed; err != nil {
		t.Fatalf("last checkpoint: %v", err)
	}
	content, _, err := store.records.load("last-observation")
	if err != nil || string(content) != "saved during shutdown" {
		t.Fatalf("checkpoint: %q %v", content, err)
	}
}

func TestRuntimeMetadataPublisherSettingsWakeOnlyOnIntervalChange(t *testing.T) {

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

	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := newTestCodespaceStateStore(t, stateDir)
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
	publisher.Run(ctx)
	active, err := publisher.ActivateRuntimeMetadata(codespaceUUID)
	if err != nil {
		t.Fatalf("activate runtime metadata: %v", err)
	}
	if !active {
		t.Fatal("runtime metadata was not activated")
	}

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

func TestRuntimeMetadataPublisherNotifyDoesNotActivate(t *testing.T) {

	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	store := newTestCodespaceStateStore(t, filepath.Join(t.TempDir(), "state"))
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(readyRuntimeMetadataSnapshot(codespaceUUID)); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}

	publisher := newRuntimeMetadataPublisher(store, controlPlane, nil, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publisher.Run(ctx)
	publisher.NotifyRuntimeMetadata(codespaceUUID)
	publisher.refreshRuntimeMetadataSet()

	if calls := service.metadataCallCount(); calls != 0 {
		t.Fatalf("metadata rpc calls = %d, want 0", calls)
	}
	publisher.mu.Lock()
	workers := len(publisher.workers)
	publisher.mu.Unlock()
	if workers != 0 {
		t.Fatalf("metadata workers = %d, want 0", workers)
	}
}

func TestRuntimeMetadataPublisherStaleOperationStopsWorker(t *testing.T) {

	connectErr := connect.NewError(connect.CodeFailedPrecondition, errors.New("stale operation"))
	detail, err := connect.NewErrorDetail(&codespacev1.FailureDetail{Category: "stale_operation"})
	if err != nil {
		t.Fatalf("create error detail: %v", err)
	}
	connectErr.AddDetail(detail)
	service := &gatewayManagerService{metadataErr: connectErr}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	store := newTestCodespaceStateStore(t, filepath.Join(t.TempDir(), "state"))
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(readyRuntimeMetadataSnapshot(codespaceUUID)); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}

	publisher := newRuntimeMetadataPublisher(store, controlPlane, nil, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publisher.Run(ctx)
	active, err := publisher.ActivateRuntimeMetadata(codespaceUUID)
	if err != nil {
		t.Fatalf("activate runtime metadata: %v", err)
	}
	if !active {
		t.Fatal("runtime metadata was not activated")
	}
	waitForMetadataCalls(t, service, 1)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _, ok, loadErr := store.LoadRuntimeMetadataRequest(codespaceUUID)
		if loadErr != nil {
			t.Fatalf("load runtime metadata: %v", loadErr)
		}
		publisher.mu.Lock()
		workers := len(publisher.workers)
		publisher.mu.Unlock()
		if !ok && workers == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_, _, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("load cleared runtime metadata: %v", err)
	}
	publisher.mu.Lock()
	workers := len(publisher.workers)
	publisher.mu.Unlock()
	if ok || workers != 0 {
		t.Fatalf("stale metadata remained active: metadata=%v workers=%d", ok, workers)
	}
	time.Sleep(10 * time.Millisecond)
	if calls := service.metadataCallCount(); calls != 1 {
		t.Fatalf("metadata rpc calls = %d, want 1", calls)
	}
}

func readyRuntimeMetadataSnapshot(codespaceUUID string) manager.RuntimeMetadataSnapshot {
	return manager.RuntimeMetadataSnapshot{
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
	}
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
