// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/manager"
)

func TestLoadRuntimeMetadataCodespaceUUIDsSkipsCleanupAndMissingMetadata(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	readyUUID := "11111111-1111-4111-8111-111111111111"
	cleanupUUID := "22222222-2222-4222-8222-222222222222"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      readyUUID,
		MetadataGeneration: 1,
		InternalSSH: manager.RuntimeMetadataInternalSSH{
			Host:               "10.0.0.12",
			Port:               2222,
			User:               "codespace",
			AuthMode:           "publickey",
			HostKeyFingerprint: "SHA256:test",
		},
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             "ready",
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save ready metadata: %v", err)
	}
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      cleanupUUID,
		MetadataGeneration: 1,
		InternalSSH: manager.RuntimeMetadataInternalSSH{
			Host:               "10.0.0.13",
			Port:               2222,
			User:               "codespace",
			AuthMode:           "publickey",
			HostKeyFingerprint: "SHA256:test",
		},
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             "ready",
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save cleanup metadata: %v", err)
	}
	if err := store.SaveCleanupPending(cleanupUUID); err != nil {
		t.Fatalf("save cleanup pending: %v", err)
	}
	if err := store.SaveRuntimeCredential("33333333-3333-4333-8333-333333333333", "runtime-token"); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}

	uuids, err := store.LoadRuntimeMetadataCodespaceUUIDs()
	if err != nil {
		t.Fatalf("load runtime metadata uuids: %v", err)
	}
	if want := []string{readyUUID}; !reflect.DeepEqual(uuids, want) {
		t.Fatalf("runtime metadata uuids = %#v, want %#v", uuids, want)
	}
}

func TestValidateCodespaceStateFilesAcceptsVersionOne(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	codespaceDir, err := codespaceStateDir(stateDir)
	if err != nil {
		t.Fatalf("codespace state dir: %v", err)
	}
	if err := os.MkdirAll(codespaceDir, 0o700); err != nil {
		t.Fatalf("create codespace state dir: %v", err)
	}
	path := filepath.Join(codespaceDir, "11111111-1111-4111-8111-111111111111.json")
	if err := os.WriteFile(path, []byte(`{"state_format_version":1}`), 0o600); err != nil {
		t.Fatalf("write codespace state: %v", err)
	}
	if err := ValidateCodespaceStateFiles(stateDir); err != nil {
		t.Fatalf("validate codespace state files: %v", err)
	}
}

func TestValidateCodespaceStateFilesAcceptsMissingDirectory(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := ValidateCodespaceStateFiles(stateDir); err != nil {
		t.Fatalf("validate missing codespace state dir: %v", err)
	}
}

func TestCodespaceStateStoreActiveOperationRoundTrip(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	operation := &codespacev1.OperationPayload{
		OperationRversion:         7,
		CodespaceUuid:             "11111111-1111-4111-8111-111111111111",
		LogOffset:                 3,
		LeaseValidForMilliseconds: 30000,
		Command: &codespacev1.OperationPayload_Create{
			Create: &codespacev1.CreateOperationPayload{
				RepoFullName:     "owner/repo",
				RepoCloneHttpUrl: "https://gitea.example.com/owner/repo.git",
				RepoCloneSshUrl:  "git@gitea.example.com:owner/repo.git",
				RepoTag:          "default",
				GitProtocol:      codespacev1.GitProtocol_GIT_PROTOCOL_HTTP,
			},
		},
	}
	if err := store.SaveActiveOperation(manager.OperationSnapshot{Payload: operation}); err != nil {
		t.Fatalf("save active operation: %v", err)
	}
	snapshots, err := store.LoadActiveOperations()
	if err != nil {
		t.Fatalf("load active operations: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d", len(snapshots))
	}
	loaded := snapshots[0].Payload
	if loaded.GetCodespaceUuid() != operation.GetCodespaceUuid() ||
		loaded.GetOperationRversion() != operation.GetOperationRversion() ||
		loaded.GetCreate().GetRepoFullName() != "owner/repo" {
		t.Fatalf("loaded operation = %#v", loaded)
	}
	if snapshots[0].WorkerStage != manager.OperationWorkerStageLeasePaused {
		t.Fatalf("worker stage = %q", snapshots[0].WorkerStage)
	}
	statePath, err := codespaceStatePath(stateDir, operation.GetCodespaceUuid())
	if err != nil {
		t.Fatalf("codespace state path: %v", err)
	}
	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read codespace state: %v", err)
	}
	var state codespaceState
	if err := json.Unmarshal(content, &state); err != nil {
		t.Fatalf("decode codespace state: %v", err)
	}
	if state.ActiveOperation == nil || state.ActiveOperation.WorkerStage != string(manager.OperationWorkerStageLeasePaused) {
		t.Fatalf("persisted worker stage = %#v", state.ActiveOperation)
	}
	if err := store.DeleteActiveOperation(operation.GetCodespaceUuid(), operation.GetOperationRversion()); err != nil {
		t.Fatalf("delete active operation: %v", err)
	}
	snapshots, err = store.LoadActiveOperations()
	if err != nil {
		t.Fatalf("reload active operations: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("snapshots after delete = %d", len(snapshots))
	}
}

func TestCodespaceStateStoreDeleteKeepsNewerOperation(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	operation := &codespacev1.OperationPayload{
		OperationRversion: 8,
		CodespaceUuid:     codespaceUUID,
		Command: &codespacev1.OperationPayload_Stop{
			Stop: &codespacev1.StopOperationPayload{},
		},
	}
	if err := store.SaveActiveOperation(manager.OperationSnapshot{Payload: operation}); err != nil {
		t.Fatalf("save active operation: %v", err)
	}
	if err := store.DeleteActiveOperation(codespaceUUID, 7); err != nil {
		t.Fatalf("delete stale active operation: %v", err)
	}
	snapshots, err := store.LoadActiveOperations()
	if err != nil {
		t.Fatalf("load active operations: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Payload.GetOperationRversion() != 8 {
		t.Fatalf("snapshots after stale delete = %#v", snapshots)
	}
}

func TestCodespaceStateStoreRuntimeTransitionPreservesActiveOperation(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	operation := &codespacev1.OperationPayload{
		OperationRversion: 8,
		CodespaceUuid:     codespaceUUID,
		Command: &codespacev1.OperationPayload_Stop{
			Stop: &codespacev1.StopOperationPayload{},
		},
	}
	if err := store.SaveActiveOperation(manager.OperationSnapshot{Payload: operation}); err != nil {
		t.Fatalf("save active operation: %v", err)
	}
	if err := store.SaveRuntimeTransitionPending(manager.RuntimeTransitionSnapshot{
		CodespaceUUID:             codespaceUUID,
		TargetState:               codespacev1.RuntimeState_RUNTIME_STATE_STOPPED,
		RuntimeGeneration:         5,
		ObservedOperationRVersion: 8,
	}); err != nil {
		t.Fatalf("save runtime transition: %v", err)
	}
	generations, err := store.LoadRuntimeGenerations()
	if err != nil {
		t.Fatalf("load runtime generations: %v", err)
	}
	if generations[codespaceUUID] != 5 {
		t.Fatalf("runtime generation = %d", generations[codespaceUUID])
	}
	transitions, err := store.LoadRuntimeTransitionPendings()
	if err != nil {
		t.Fatalf("load runtime transition pendings: %v", err)
	}
	if len(transitions) != 1 ||
		transitions[0].CodespaceUUID != codespaceUUID ||
		transitions[0].RuntimeGeneration != 5 ||
		transitions[0].TargetState != codespacev1.RuntimeState_RUNTIME_STATE_STOPPED {
		t.Fatalf("runtime transition pendings = %#v", transitions)
	}
	statePath, err := codespaceStatePath(stateDir, codespaceUUID)
	if err != nil {
		t.Fatalf("codespace state path: %v", err)
	}
	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read codespace state: %v", err)
	}
	var state codespaceState
	if err := json.Unmarshal(content, &state); err != nil {
		t.Fatalf("decode codespace state: %v", err)
	}
	if state.ActiveOperation == nil || state.PendingRuntimeTransition == nil {
		t.Fatalf("state after runtime transition = %#v", state)
	}
	if err := store.DeleteActiveOperation(codespaceUUID, 8); err != nil {
		t.Fatalf("delete active operation: %v", err)
	}
	state, err = loadCodespaceStateFile(statePath, codespaceUUID)
	if err != nil {
		t.Fatalf("load codespace state after active delete: %v", err)
	}
	if state.ActiveOperation != nil ||
		state.RuntimeGeneration != 5 ||
		state.PendingRuntimeTransition == nil {
		t.Fatalf("state after active delete = %#v", state)
	}
	if err := store.ClearRuntimeTransitionPending(codespaceUUID, 5); err != nil {
		t.Fatalf("clear runtime transition: %v", err)
	}
	state, err = loadCodespaceStateFile(statePath, codespaceUUID)
	if err != nil {
		t.Fatalf("load codespace state after transition clear: %v", err)
	}
	if state.RuntimeGeneration != 5 || state.PendingRuntimeTransition != nil {
		t.Fatalf("state after transition clear = %#v", state)
	}
}

func TestCodespaceStateStoreCleanupPendingSkipsOperationRecovery(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	operation := &codespacev1.OperationPayload{
		OperationRversion: 8,
		CodespaceUuid:     codespaceUUID,
		Command: &codespacev1.OperationPayload_Stop{
			Stop: &codespacev1.StopOperationPayload{},
		},
	}
	if err := store.SaveActiveOperation(manager.OperationSnapshot{Payload: operation}); err != nil {
		t.Fatalf("save active operation: %v", err)
	}
	if err := store.SaveCleanupPending(codespaceUUID); err != nil {
		t.Fatalf("save cleanup pending: %v", err)
	}
	snapshots, err := store.LoadActiveOperations()
	if err != nil {
		t.Fatalf("load active operations: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("snapshots under cleanup pending = %#v", snapshots)
	}
	cleanupPendings, err := store.LoadCleanupPendings()
	if err != nil {
		t.Fatalf("load cleanup pendings: %v", err)
	}
	if len(cleanupPendings) != 1 || cleanupPendings[0] != codespaceUUID {
		t.Fatalf("cleanup pendings = %#v", cleanupPendings)
	}
	statePath, err := codespaceStatePath(stateDir, codespaceUUID)
	if err != nil {
		t.Fatalf("codespace state path: %v", err)
	}
	state, err := loadCodespaceStateFile(statePath, codespaceUUID)
	if err != nil {
		t.Fatalf("load cleanup state: %v", err)
	}
	if !state.CleanupPending {
		t.Fatalf("cleanup pending was not saved: %#v", state)
	}
	if err := store.ClearCodespaceState(codespaceUUID); err != nil {
		t.Fatalf("clear codespace state: %v", err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file after clear err = %v", err)
	}
}

func TestCodespaceStateStoreEndpointRouteRoundTrip(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveEndpointRoute(gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     "app-3000",
		label:          " App 3000 ",
		upstreamScheme: "HTTP",
		upstreamHost:   "127.0.0.1:3000",
		public:         true,
	}); err != nil {
		t.Fatalf("save endpoint route: %v", err)
	}
	routes, err := store.LoadGatewayRoutes()
	if err != nil {
		t.Fatalf("load gateway routes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes = %#v", routes)
	}
	route := routes[0]
	if route.codespaceUUID != codespaceUUID ||
		route.endpointID != "app-3000" ||
		route.label != "App 3000" ||
		route.upstreamScheme != "http" ||
		route.upstreamHost != "127.0.0.1:3000" ||
		!route.public {
		t.Fatalf("route = %#v", route)
	}

	if err := store.DeleteEndpointRoute(codespaceUUID, "app-3000"); err != nil {
		t.Fatalf("delete endpoint route: %v", err)
	}
	routes, err = store.LoadGatewayRoutes()
	if err != nil {
		t.Fatalf("reload gateway routes: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("routes after delete = %#v", routes)
	}
	statePath, err := codespaceStatePath(stateDir, codespaceUUID)
	if err != nil {
		t.Fatalf("codespace state path: %v", err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file after route delete err = %v", err)
	}
}

func TestCodespaceStateStoreEndpointRoutePreservesRuntimeState(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeTransitionPending(manager.RuntimeTransitionSnapshot{
		CodespaceUUID:             codespaceUUID,
		TargetState:               codespacev1.RuntimeState_RUNTIME_STATE_STOPPED,
		RuntimeGeneration:         5,
		ObservedOperationRVersion: 8,
	}); err != nil {
		t.Fatalf("save runtime transition: %v", err)
	}
	if err := store.SaveEndpointRoute(gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     "workspace",
		label:          "Workspace",
		upstreamScheme: "http",
		upstreamHost:   "localhost:8080",
	}); err != nil {
		t.Fatalf("save endpoint route: %v", err)
	}
	if err := store.DeleteEndpointRoute(codespaceUUID, "workspace"); err != nil {
		t.Fatalf("delete endpoint route: %v", err)
	}
	generations, err := store.LoadRuntimeGenerations()
	if err != nil {
		t.Fatalf("load runtime generations: %v", err)
	}
	if generations[codespaceUUID] != 5 {
		t.Fatalf("runtime generation after endpoint delete = %d", generations[codespaceUUID])
	}
}

func TestCodespaceStateStoreEndpointLimitAllowsUpdates(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	for i := 0; i < maxCodespaceEndpoints; i++ {
		if err := store.SaveEndpointRoute(gatewayEndpointRoute{
			codespaceUUID:  codespaceUUID,
			endpointID:     endpointIDForTest(i),
			label:          "App",
			upstreamScheme: "http",
			upstreamHost:   "127.0.0.1:3000",
		}); err != nil {
			t.Fatalf("save endpoint %d: %v", i, err)
		}
	}
	if err := store.SaveEndpointRoute(gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     "extra",
		label:          "Extra",
		upstreamScheme: "http",
		upstreamHost:   "127.0.0.1:3001",
	}); !errors.Is(err, errEndpointLimitExceeded) {
		t.Fatalf("extra endpoint err = %v", err)
	}
	if err := store.SaveEndpointRoute(gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     endpointIDForTest(0),
		label:          "Updated",
		upstreamScheme: "http",
		upstreamHost:   "127.0.0.1:3002",
	}); err != nil {
		t.Fatalf("update existing endpoint at limit: %v", err)
	}
}

func TestCodespaceStateStoreRuntimeMetadataRequestIncludesEndpoints(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InternalSSH: manager.RuntimeMetadataInternalSSH{
			Host:               "10.0.0.12",
			Port:               2222,
			User:               "codespace",
			AuthMode:           "publickey",
			HostKeyFingerprint: "SHA256:test",
		},
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             "ready",
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata snapshot: %v", err)
	}
	if err := store.SaveEndpointRoute(gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     "app-3000",
		label:          "App 3000",
		upstreamScheme: "http",
		upstreamHost:   "10.0.0.12:3000",
		public:         true,
	}); err != nil {
		t.Fatalf("save endpoint route: %v", err)
	}
	generation, metadataJSON, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("load runtime metadata request: %v", err)
	}
	if !ok || generation != 2 {
		t.Fatalf("metadata ok=%v generation=%d", ok, generation)
	}
	if !strings.Contains(metadataJSON, `"endpoint_id":"app-3000"`) ||
		!strings.Contains(metadataJSON, `"label":"App 3000"`) ||
		!strings.Contains(metadataJSON, `"public":true`) ||
		!strings.Contains(metadataJSON, `"stage":"ready"`) {
		t.Fatalf("metadata json = %s", metadataJSON)
	}
	if err := store.SaveEndpointRoute(gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     "app-3000",
		label:          "App 3000",
		upstreamScheme: "http",
		upstreamHost:   "10.0.0.12:3000",
		public:         true,
	}); err != nil {
		t.Fatalf("resave endpoint route: %v", err)
	}
	generation, _, ok, err = store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("reload runtime metadata request: %v", err)
	}
	if !ok || generation != 2 {
		t.Fatalf("metadata after same save ok=%v generation=%d", ok, generation)
	}
}

func TestCodespaceStateStoreRuntimeMetadataAllowsNonReadyWithoutInternalSSH(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStagePublishRuntime,
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save non-ready runtime metadata snapshot: %v", err)
	}
	_, metadataJSON, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("load runtime metadata request: %v", err)
	}
	if !ok {
		t.Fatalf("metadata request was missing")
	}
	if strings.Contains(metadataJSON, "internal_ssh") || !strings.Contains(metadataJSON, `"stage":"publish-runtime"`) {
		t.Fatalf("metadata json = %s", metadataJSON)
	}
}

func TestCodespaceStateStoreRuntimeMetadataReadyRequiresInternalSSH(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      "11111111-1111-4111-8111-111111111111",
		MetadataGeneration: 1,
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "ready runtime metadata requires internal ssh") {
		t.Fatalf("ready metadata err = %v", err)
	}
}

func TestCodespaceStateStoreRuntimeAPIOperation(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	operation, err := store.RuntimeAPIOperation(codespaceUUID)
	if err != nil {
		t.Fatalf("runtime api operation without state: %v", err)
	}
	if operation != runtimeAPIOperationNone {
		t.Fatalf("operation without state = %q", operation)
	}
	if err := store.SaveActiveOperation(manager.OperationSnapshot{Payload: &codespacev1.OperationPayload{
		OperationRversion: 1,
		CodespaceUuid:     codespaceUUID,
		Command: &codespacev1.OperationPayload_Create{
			Create: &codespacev1.CreateOperationPayload{},
		},
	}}); err != nil {
		t.Fatalf("save active operation: %v", err)
	}
	operation, err = store.RuntimeAPIOperation(codespaceUUID)
	if err != nil {
		t.Fatalf("runtime api operation with create: %v", err)
	}
	if operation != runtimeAPIOperationCreate {
		t.Fatalf("operation with create = %q", operation)
	}
	if err := store.SaveCleanupPending(codespaceUUID); err != nil {
		t.Fatalf("save cleanup pending: %v", err)
	}
	operation, err = store.RuntimeAPIOperation(codespaceUUID)
	if err != nil {
		t.Fatalf("runtime api operation with cleanup: %v", err)
	}
	if operation != runtimeAPIOperationDelete {
		t.Fatalf("operation with cleanup = %q", operation)
	}
}

func endpointIDForTest(index int) string {
	return fmt.Sprintf("app-%02d", index)
}

func TestCodespaceStateStoreRuntimeCredentialRoundTrip(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	statePath, err := codespaceStatePath(stateDir, codespaceUUID)
	if err != nil {
		t.Fatalf("codespace state path: %v", err)
	}
	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if strings.Contains(string(content), token) {
		t.Fatalf("runtime token was stored in clear text: %s", content)
	}
	resolved, ok, err := store.ResolveRuntimeToken(token)
	if err != nil {
		t.Fatalf("resolve runtime token: %v", err)
	}
	if !ok || resolved != codespaceUUID {
		t.Fatalf("resolved=%q ok=%v", resolved, ok)
	}
	if _, ok, err := store.ResolveRuntimeToken("wrong-token"); err != nil || ok {
		t.Fatalf("wrong token ok=%v err=%v", ok, err)
	}
}

func TestValidateCodespaceStateFilesRejectsInvalidEndpointSnapshot(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	codespaceDir, err := codespaceStateDir(stateDir)
	if err != nil {
		t.Fatalf("codespace state dir: %v", err)
	}
	if err := os.MkdirAll(codespaceDir, 0o700); err != nil {
		t.Fatalf("create codespace state dir: %v", err)
	}
	path := filepath.Join(codespaceDir, "11111111-1111-4111-8111-111111111111.json")
	if err := os.WriteFile(path, []byte(`{
		"state_format_version": 1,
		"endpoints": [
			{"endpoint_id": "workspace", "label": "Bad\u003cLabel", "upstream_scheme": "http", "upstream_host": "127.0.0.1:3000", "public": false}
		]
	}`), 0o600); err != nil {
		t.Fatalf("write codespace state: %v", err)
	}
	if err := ValidateCodespaceStateFiles(stateDir); err == nil {
		t.Fatalf("expected invalid endpoint snapshot error")
	}
}

func TestValidateCodespaceStateFilesRejectsTooManyEndpoints(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	codespaceDir, err := codespaceStateDir(stateDir)
	if err != nil {
		t.Fatalf("codespace state dir: %v", err)
	}
	if err := os.MkdirAll(codespaceDir, 0o700); err != nil {
		t.Fatalf("create codespace state dir: %v", err)
	}
	state := codespaceState{
		StateFormatVersion: codespaceStateFormatVersion,
		Endpoints:          make([]codespaceEndpointSnapshot, 0, maxCodespaceEndpoints+1),
	}
	for i := 0; i <= maxCodespaceEndpoints; i++ {
		state.Endpoints = append(state.Endpoints, codespaceEndpointSnapshot{
			EndpointID:     endpointIDForTest(i),
			Label:          "App",
			UpstreamScheme: "http",
			UpstreamHost:   "127.0.0.1:3000",
		})
	}
	content, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	path := filepath.Join(codespaceDir, "11111111-1111-4111-8111-111111111111.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write codespace state: %v", err)
	}
	if err := ValidateCodespaceStateFiles(stateDir); err == nil {
		t.Fatalf("expected too many endpoints error")
	}
}

func TestCodespaceStateStoreRejectsInvalidWorkerStage(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	operation := &codespacev1.OperationPayload{
		OperationRversion: 9,
		CodespaceUuid:     "11111111-1111-4111-8111-111111111111",
		Command: &codespacev1.OperationPayload_Stop{
			Stop: &codespacev1.StopOperationPayload{},
		},
	}
	err := store.SaveActiveOperation(manager.OperationSnapshot{
		Payload:     operation,
		WorkerStage: manager.OperationWorkerStage("unknown"),
	})
	if err == nil {
		t.Fatalf("expected invalid worker stage error")
	}
}

func TestValidateCodespaceStateFilesRejectsWrongFormat(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	codespaceDir, err := codespaceStateDir(stateDir)
	if err != nil {
		t.Fatalf("codespace state dir: %v", err)
	}
	if err := os.MkdirAll(codespaceDir, 0o700); err != nil {
		t.Fatalf("create codespace state dir: %v", err)
	}
	path := filepath.Join(codespaceDir, "11111111-1111-4111-8111-111111111111.json")
	if err := os.WriteFile(path, []byte(`{"state_format_version":2}`), 0o600); err != nil {
		t.Fatalf("write codespace state: %v", err)
	}
	if err := ValidateCodespaceStateFiles(stateDir); err == nil {
		t.Fatalf("expected wrong format error")
	}
}

func TestValidateCodespaceStateFilesRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	codespaceDir, err := codespaceStateDir(stateDir)
	if err != nil {
		t.Fatalf("codespace state dir: %v", err)
	}
	if err := os.MkdirAll(codespaceDir, 0o700); err != nil {
		t.Fatalf("create codespace state dir: %v", err)
	}
	path := filepath.Join(codespaceDir, "11111111-1111-4111-8111-111111111111.json")
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatalf("write codespace state: %v", err)
	}
	if err := ValidateCodespaceStateFiles(stateDir); err == nil {
		t.Fatalf("expected invalid json error")
	}
}

func TestValidateCodespaceStateFilesRejectsInvalidName(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	codespaceDir, err := codespaceStateDir(stateDir)
	if err != nil {
		t.Fatalf("codespace state dir: %v", err)
	}
	if err := os.MkdirAll(codespaceDir, 0o700); err != nil {
		t.Fatalf("create codespace state dir: %v", err)
	}
	path := filepath.Join(codespaceDir, "not-a-uuid.json")
	if err := os.WriteFile(path, []byte(`{"state_format_version":1}`), 0o600); err != nil {
		t.Fatalf("write codespace state: %v", err)
	}
	if err := ValidateCodespaceStateFiles(stateDir); err == nil {
		t.Fatalf("expected invalid name error")
	}
}

func TestRunWithConfigInvalidCodespaceStateFailsBeforeRPC(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	writeRunnableState(t, stateDir)
	codespaceDir, err := codespaceStateDir(stateDir)
	if err != nil {
		t.Fatalf("codespace state dir: %v", err)
	}
	if err := os.MkdirAll(codespaceDir, 0o700); err != nil {
		t.Fatalf("create codespace state dir: %v", err)
	}
	path := filepath.Join(codespaceDir, "11111111-1111-4111-8111-111111111111.json")
	if err := os.WriteFile(path, []byte(`{"state_format_version":2}`), 0o600); err != nil {
		t.Fatalf("write codespace state: %v", err)
	}

	service := &lockTestManagerService{}
	server := newLockTestManagerServer(t, service)
	defer server.Close()

	var output bytes.Buffer
	config := DefaultConfig()
	config.Server.ListenAddr = "127.0.0.1:0"
	config.Gitea.URL = server.URL
	config.Manager.StateDir = stateDir
	config.Manager.HTTPTimeout = Duration(100 * time.Millisecond)
	err = RunWithConfig(&output, config)
	if err == nil {
		t.Fatalf("expected invalid codespace state error")
	}
	if !strings.Contains(err.Error(), "state_format_version") {
		t.Fatalf("unexpected error: %v", err)
	}
	if service.calls.Load() != 0 {
		t.Fatalf("manager service calls = %d", service.calls.Load())
	}
}

func writeRunnableState(t *testing.T, stateDir string) {
	t.Helper()
	if err := SaveManagerCredentials(stateDir, ManagerCredentials{
		ManagerID:     42,
		ManagerSecret: "manager-secret",
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	if err := SaveManagerRootState(stateDir, ManagerRootState{
		ManagerID: 42,
	}); err != nil {
		t.Fatalf("save root state: %v", err)
	}
}

func newLockTestManagerServer(t *testing.T, service *lockTestManagerService) *httptest.Server {
	t.Helper()
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	return httptest.NewServer(mux)
}
