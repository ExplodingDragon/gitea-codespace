// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

func completeEndpointRoutesForTest(codespaceUUID string, routes ...manager.RuntimeEndpointRoute) []manager.RuntimeEndpointRoute {
	instanceName := "runtime-1"
	if len(routes) > 0 && routes[0].InstanceName != "" {
		instanceName = routes[0].InstanceName
	}
	return append([]manager.RuntimeEndpointRoute{{
		CodespaceUUID:  codespaceUUID,
		EndpointID:     runtimeendpoint.WorkspaceEndpointID,
		Label:          runtimeendpoint.WorkspaceEndpointLabel,
		UpstreamScheme: "http",
		InstanceName:   instanceName,
		UpstreamPort:   runtimeendpoint.WorkspaceEndpointPort,
	}}, routes...)
}

func TestValidateCodespaceStateFilesAcceptsCurrentVersion(t *testing.T) {
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
	if err := os.WriteFile(path, []byte(`{"state_format_version":3}`), 0o600); err != nil {
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
				EnvironmentTag:   "default",
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

func TestCodespaceStateStoreSavesRuntimeEnvironment(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	want := runtimeEnvironmentForTest(codespaceUUID)
	if err := store.SaveRuntimeEnvironment(codespaceUUID, want); err != nil {
		t.Fatalf("save runtime environment: %v", err)
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
	if state.RuntimeEnvironment == nil || state.RuntimeEnvironment.User != want.User || state.RuntimeEnvironment.Environment.PrimaryContainerID != want.Environment.PrimaryContainerID {
		t.Fatalf("runtime environment = %#v", state.RuntimeEnvironment)
	}
	loaded, ok, err := store.LoadRuntimeEnvironment(codespaceUUID)
	if err != nil {
		t.Fatalf("load runtime environment: %v", err)
	}
	if !ok || loaded.User != want.User || loaded.Group != want.Group || loaded.Environment.PrimaryContainerID != want.Environment.PrimaryContainerID {
		t.Fatalf("loaded runtime environment ok=%v value=%#v", ok, loaded)
	}
}

func runtimeEnvironmentForTest(codespaceUUID string) provisioner.RuntimeEnvironment {
	return provisioner.RuntimeEnvironment{
		User:  1000,
		Group: 1000,
		Environment: devcontainer.State{
			Version:             devcontainer.StateFormatVersion,
			ID:                  "environment-id",
			OwnerID:             codespaceUUID,
			ConfigurationPath:   "/workspaces/repo/.devcontainer/devcontainer.json",
			ConfigurationSHA256: strings.Repeat("a", 64),
			Workspace:           "/workspaces/repo",
			WorkspaceFolder:     "/workspaces/repo",
			PrimaryContainerID:  "container-id",
			RemoteUser:          "developer",
			RemoteWorkdir:       "/workspaces/repo",
		},
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

func TestCodespaceStateStoreHealthStopPendingRoundTrip(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/codespace/owner/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata snapshot: %v", err)
	}
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, completeEndpointRoutesForTest(codespaceUUID, manager.RuntimeEndpointRoute{
		EndpointID:     "app",
		Label:          "App",
		UpstreamScheme: "http",
		InstanceName:   "runtime-1",
		UpstreamPort:   3000,
	})); err != nil {
		t.Fatalf("save endpoint routes: %v", err)
	}
	if err := store.SaveHealthStopPending(manager.HealthStopSnapshot{
		CodespaceUUID:             codespaceUUID,
		ObservedOperationRVersion: 7,
	}); err != nil {
		t.Fatalf("save health stop pending: %v", err)
	}
	if err := store.ClearRuntimeMetadata(codespaceUUID); err != nil {
		t.Fatalf("clear runtime metadata: %v", err)
	}

	pendings, err := store.LoadHealthStopPendings()
	if err != nil {
		t.Fatalf("load health stop pendings: %v", err)
	}
	if len(pendings) != 1 || pendings[0].CodespaceUUID != codespaceUUID || pendings[0].ObservedOperationRVersion != 7 {
		t.Fatalf("health stop pendings = %#v", pendings)
	}
	if routes, err := store.LoadGatewayRoutes(); err != nil || len(routes) != 0 {
		t.Fatalf("gateway routes err=%v routes=%#v", err, routes)
	}
	if target, ok, err := store.LoadGatewayWorkspaceTarget(codespaceUUID); err != nil || ok {
		t.Fatalf("workspace target err=%v ok=%v target=%#v", err, ok, target)
	}
	if err := store.SaveRuntimeTransitionPending(manager.RuntimeTransitionSnapshot{
		CodespaceUUID:             codespaceUUID,
		TargetState:               codespacev1.RuntimeState_RUNTIME_STATE_STOPPED,
		RuntimeGeneration:         1,
		ObservedOperationRVersion: 7,
	}); err != nil {
		t.Fatalf("save runtime transition pending: %v", err)
	}
	statePath, err := codespaceStatePath(stateDir, codespaceUUID)
	if err != nil {
		t.Fatalf("codespace state path: %v", err)
	}
	state, err := loadCodespaceStateFile(statePath, codespaceUUID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.HealthStopPending || state.HealthStopObservedOperationRVersion != 0 || state.PendingRuntimeTransition == nil {
		t.Fatalf("state = %#v", state)
	}
}

func TestCodespaceStateStoreEndpointRouteRoundTrip(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	changed, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, completeEndpointRoutesForTest(codespaceUUID, manager.RuntimeEndpointRoute{
		EndpointID:     "app-3000",
		Label:          " App 3000 ",
		UpstreamScheme: "HTTP",
		InstanceName:   "runtime-1",
		UpstreamPort:   3000,
		Public:         true,
	}))
	if err != nil {
		t.Fatalf("save endpoint route: %v", err)
	}
	if !changed {
		t.Fatalf("endpoint route save was not marked changed")
	}
	routes, err := store.LoadGatewayRoutes()
	if err != nil {
		t.Fatalf("load gateway routes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %#v", routes)
	}
	route := routes[0]
	if route.codespaceUUID != codespaceUUID ||
		route.endpointID != "app-3000" ||
		route.label != "App 3000" ||
		route.upstreamScheme != "http" ||
		route.instanceName != "runtime-1" || route.upstreamPort != 3000 ||
		!route.public {
		t.Fatalf("route = %#v", route)
	}

	changed, err = store.SaveRuntimeEndpointRoutes(codespaceUUID, nil)
	if err != nil {
		t.Fatalf("clear endpoint routes: %v", err)
	}
	if !changed {
		t.Fatalf("endpoint route clear was not marked changed")
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

func TestValidateEndpointLabel(t *testing.T) {
	t.Parallel()

	valid64Runes := strings.Repeat("界", 64)
	invalid65Runes := strings.Repeat("a", 65)
	tests := []struct {
		name  string
		label string
		valid bool
	}{
		{name: "trimmed ascii", label: " App 3000 ", valid: true},
		{name: "one rune", label: "A", valid: true},
		{name: "sixty four unicode runes", label: valid64Runes, valid: true},
		{name: "chinese", label: "预览服务", valid: true},
		{name: "empty after trim", label: " \t\n ", valid: false},
		{name: "invalid utf8", label: string([]byte{0xff, 'A'}), valid: false},
		{name: "sixty five runes", label: invalid65Runes, valid: false},
		{name: "control character", label: "Bad\nLabel", valid: false},
		{name: "less than", label: "Bad<Label", valid: false},
		{name: "greater than", label: "Bad>Label", valid: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateEndpointLabel(test.label)
			if test.valid && err != nil {
				t.Fatalf("expected valid label, got %v", err)
			}
			if !test.valid && err == nil {
				t.Fatalf("expected invalid label")
			}
		})
	}
}

func TestCodespaceStateStoreAllowsDuplicateEndpointLabels(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	var endpointRoutes []manager.RuntimeEndpointRoute
	for _, endpointID := range []string{"app-3000", "app-3001"} {
		endpointRoutes = append(endpointRoutes, manager.RuntimeEndpointRoute{
			EndpointID:     endpointID,
			Label:          "预览服务",
			UpstreamScheme: "http",
			InstanceName:   "runtime-1",
			UpstreamPort:   3000,
		})
	}
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, completeEndpointRoutesForTest(codespaceUUID, endpointRoutes...)); err != nil {
		t.Fatalf("save endpoint routes: %v", err)
	}
	routes, err := store.LoadGatewayRoutes()
	if err != nil {
		t.Fatalf("load gateway routes: %v", err)
	}
	if len(routes) != 3 || routes[0].label != "预览服务" || routes[1].label != "预览服务" {
		t.Fatalf("routes = %#v", routes)
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
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, completeEndpointRoutesForTest(codespaceUUID, manager.RuntimeEndpointRoute{
		EndpointID:     "web",
		Label:          "Web",
		UpstreamScheme: "http",
		InstanceName:   "runtime-1",
		UpstreamPort:   8080,
	})); err != nil {
		t.Fatalf("save endpoint route: %v", err)
	}
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, nil); err != nil {
		t.Fatalf("clear endpoint routes: %v", err)
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
	routes := completeEndpointRoutesForTest(codespaceUUID)
	for i := 0; i < maxCodespaceEndpoints-1; i++ {
		routes = append(routes, runtimeEndpointRouteForTest(endpointIDForTest(i), "App", "127.0.0.1:3000"))
	}
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, routes); err != nil {
		t.Fatalf("save max endpoint routes: %v", err)
	}
	tooManyRoutes := append([]manager.RuntimeEndpointRoute(nil), routes...)
	tooManyRoutes = append(tooManyRoutes, runtimeEndpointRouteForTest("extra", "Extra", "127.0.0.1:3001"))
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, tooManyRoutes); !errors.Is(err, errEndpointLimitExceeded) {
		t.Fatalf("extra endpoint err = %v", err)
	}
	routes[1] = runtimeEndpointRouteForTest(endpointIDForTest(0), "Updated", "127.0.0.1:3002")
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, routes); err != nil {
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
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             "ready",
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata snapshot: %v", err)
	}
	endpointRoutes := completeEndpointRoutesForTest(codespaceUUID, manager.RuntimeEndpointRoute{
		EndpointID:     "app-3000",
		Label:          "App 3000",
		UpstreamScheme: "http",
		InstanceName:   "runtime-1",
		UpstreamPort:   3000,
		Public:         true,
	})
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, endpointRoutes); err != nil {
		t.Fatalf("save endpoint route: %v", err)
	}
	generation, metadata, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("load runtime metadata request: %v", err)
	}
	if !ok || generation != 2 {
		t.Fatalf("metadata ok=%v generation=%d", ok, generation)
	}
	endpoints := metadata.GetEndpoints()
	if len(endpoints) != 2 ||
		endpoints[0].GetEndpointId() != "app-3000" ||
		endpoints[0].GetLabel() != "App 3000" ||
		!endpoints[0].GetPublic() ||
		endpoints[1].GetEndpointId() != runtimeendpoint.WorkspaceEndpointID ||
		metadata.GetBoot().GetStage() != codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY {
		t.Fatalf("metadata = %#v", metadata)
	}
	changed, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, endpointRoutes)
	if err != nil {
		t.Fatalf("resave endpoint route: %v", err)
	}
	if changed {
		t.Fatalf("same endpoint route was marked changed")
	}
	generation, _, ok, err = store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("reload runtime metadata request: %v", err)
	}
	if !ok || generation != 2 {
		t.Fatalf("metadata after same save ok=%v generation=%d", ok, generation)
	}
}

func TestCodespaceStateStoreLoadsRuntimeMetadataSnapshot(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	snapshot := manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 3,
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}
	if err := store.SaveRuntimeMetadataSnapshot(snapshot); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}
	loaded, ok, err := store.LoadRuntimeMetadataSnapshot(codespaceUUID)
	if err != nil {
		t.Fatalf("load runtime metadata snapshot: %v", err)
	}
	if !ok || !reflect.DeepEqual(loaded, snapshot) {
		t.Fatalf("loaded snapshot ok=%v value=%#v", ok, loaded)
	}
	if err := store.SaveCleanupPending(codespaceUUID); err != nil {
		t.Fatalf("save cleanup pending: %v", err)
	}
	_, ok, err = store.LoadRuntimeMetadataSnapshot(codespaceUUID)
	if err != nil {
		t.Fatalf("load cleanup snapshot: %v", err)
	}
	if ok {
		t.Fatalf("cleanup pending snapshot was returned")
	}
}

func TestCodespaceStateStoreClearRuntimeMetadataKeepsResumeState(t *testing.T) {
	t.Parallel()

	store := NewCodespaceStateStore(filepath.Join(t.TempDir(), "state"))
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	input := manager.StartupInput{
		CodespaceUUID:   codespaceUUID,
		RepoFullName:    "owner/repo",
		Username:        "developer",
		GitUserEmail:    "developer@example.com",
		RuntimeUserName: "developer",
		EnvironmentTag:  "default",
		DevContainer: provisioner.DevContainerConfiguration{
			Source:       "platform_default",
			DefaultImage: "mcr.microsoft.com/devcontainers/base:ubuntu",
		},
	}
	if err := store.SaveStartupInput(input); err != nil {
		t.Fatalf("save startup input: %v", err)
	}
	environment := runtimeEnvironmentForTest(codespaceUUID)
	if err := store.SaveRuntimeEnvironment(codespaceUUID, environment); err != nil {
		t.Fatalf("save runtime environment: %v", err)
	}
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
		t.Fatalf("save runtime metadata: %v", err)
	}
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, completeEndpointRoutesForTest(codespaceUUID, manager.RuntimeEndpointRoute{
		EndpointID:     "app",
		Label:          "App",
		UpstreamScheme: "http",
		InstanceName:   "runtime-1",
		UpstreamPort:   3000,
	})); err != nil {
		t.Fatalf("save endpoint route: %v", err)
	}

	if err := store.ClearRuntimeMetadata(codespaceUUID); err != nil {
		t.Fatalf("clear runtime metadata: %v", err)
	}
	if _, _, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID); err != nil || ok {
		t.Fatalf("runtime metadata after clear ok=%v err=%v", ok, err)
	}
	if routes, err := store.LoadGatewayRoutes(); err != nil || len(routes) != 0 {
		t.Fatalf("gateway routes after clear = %#v, err=%v", routes, err)
	}
	lateRoute := manager.RuntimeEndpointRoute{
		EndpointID:     "late",
		Label:          "Late",
		UpstreamScheme: "http",
		InstanceName:   "runtime-1",
		UpstreamPort:   4000,
	}
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, completeEndpointRoutesForTest(codespaceUUID, lateRoute)); err == nil {
		t.Fatal("late endpoint update was accepted after runtime metadata clear")
	}
	if loaded, ok, err := store.LoadStartupInput(codespaceUUID); err != nil || !ok || !reflect.DeepEqual(loaded, input) {
		t.Fatalf("startup input after clear ok=%v value=%#v err=%v", ok, loaded, err)
	}
	if loaded, ok, err := store.LoadRuntimeEnvironment(codespaceUUID); err != nil || !ok || loaded.User != environment.User || loaded.Environment.PrimaryContainerID != environment.Environment.PrimaryContainerID {
		t.Fatalf("runtime environment after clear ok=%v value=%#v err=%v", ok, loaded, err)
	}
	resumed := manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 3,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 8,
			Stage:             manager.RuntimeBootStagePrepareRuntime,
			StartedUnix:       12,
			LastUpdateUnix:    12,
		},
	}
	if err := store.SaveRuntimeMetadataSnapshot(resumed); err != nil {
		t.Fatalf("save resumed runtime metadata: %v", err)
	}
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, completeEndpointRoutesForTest(codespaceUUID, lateRoute)); err != nil {
		t.Fatalf("save endpoint after runtime metadata resume: %v", err)
	}
}

func TestCodespaceStateStoreClearRuntimeMetadataWinsConcurrentUsageUpdates(t *testing.T) {
	t.Parallel()

	store := NewCodespaceStateStore(filepath.Join(t.TempDir(), "state"))
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
		t.Fatalf("save runtime metadata: %v", err)
	}

	start := make(chan struct{})
	done := make(chan error, 16)
	for i := int64(0); i < 16; i++ {
		go func(observedUnix int64) {
			<-start
			_, err := store.UpdateRuntimeResourceUsage(codespaceUUID, provisioner.RuntimeResourceUsage{ObservedUnix: observedUnix})
			done <- err
		}(i + 1)
	}
	close(start)
	if err := store.ClearRuntimeMetadata(codespaceUUID); err != nil {
		t.Fatalf("clear runtime metadata: %v", err)
	}
	for range 16 {
		if err := <-done; err != nil {
			t.Fatalf("update runtime resource usage: %v", err)
		}
	}
	if _, _, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID); err != nil || ok {
		t.Fatalf("runtime metadata after concurrent clear ok=%v err=%v", ok, err)
	}
}

func TestCodespaceStateStoreRuntimeMetadataIncludesBootDuringStartup(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStagePublishReady,
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save non-ready runtime metadata snapshot: %v", err)
	}
	_, metadata, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("load runtime metadata request: %v", err)
	}
	if !ok {
		t.Fatalf("metadata request was missing")
	}
	if metadata.GetBoot().GetStage() != codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PUBLISH_READY {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestCodespaceStateStoreRuntimeMetadataReadyKeepsRoutingLocal(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      "11111111-1111-4111-8111-111111111111",
		MetadataGeneration: 1,
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save ready runtime metadata: %v", err)
	}
	_, metadata, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("load ready runtime metadata request: %v", err)
	}
	if !ok {
		t.Fatalf("metadata request was missing")
	}
	if metadata.GetBoot().GetStage() != codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestCodespaceStateStoreClosesSessionsWhenWorkspaceTargetChanges(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	sessions := newGatewaySessionRegistry()
	store.SetSessionRegistry(sessions)
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
		t.Fatalf("save first runtime metadata snapshot: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	release := sessions.BeginCancelable(codespaceUUID, cancel)
	defer release()
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 2,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/other",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       10,
			LastUpdateUnix:    12,
		},
	}); err != nil {
		t.Fatalf("save changed runtime metadata snapshot: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatalf("session was not canceled after workspace target changed")
	}
}

func TestCodespaceStateStoreRebasesRuntimeMetadataGeneration(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 2,
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata snapshot: %v", err)
	}
	if err := store.RebaseRuntimeMetadataGeneration(codespaceUUID, 5); err != nil {
		t.Fatalf("rebase runtime metadata generation: %v", err)
	}
	generation, metadata, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("load runtime metadata request: %v", err)
	}
	if !ok || generation != 6 || metadata.GetBoot().GetStage() != codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY {
		t.Fatalf("metadata generation=%d ok=%v metadata=%#v", generation, ok, metadata)
	}
	if err := store.RebaseRuntimeMetadataGeneration(codespaceUUID, 3); err != nil {
		t.Fatalf("rebase with older server generation: %v", err)
	}
	generation, _, ok, err = store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("reload runtime metadata request: %v", err)
	}
	if !ok || generation != 6 {
		t.Fatalf("metadata after older rebase generation=%d ok=%v", generation, ok)
	}
}

func endpointIDForTest(index int) string {
	return fmt.Sprintf("app-%02d", index)
}

func runtimeEndpointRouteForTest(endpointID, label, _ string) manager.RuntimeEndpointRoute {
	return manager.RuntimeEndpointRoute{
		EndpointID:     endpointID,
		Label:          label,
		UpstreamScheme: "http",
		InstanceName:   "runtime-1",
		UpstreamPort:   3000,
	}
}

func TestCodespaceStateStoreRuntimeEndpointRoutesReplaceSnapshot(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata snapshot: %v", err)
	}
	routes := completeEndpointRoutesForTest(codespaceUUID,
		manager.RuntimeEndpointRoute{
			CodespaceUUID:  codespaceUUID,
			EndpointID:     "web",
			Label:          "Web",
			UpstreamScheme: "http",
			InstanceName:   "runtime-1",
			UpstreamPort:   3000,
			Public:         true,
		},
		manager.RuntimeEndpointRoute{
			CodespaceUUID:  codespaceUUID,
			EndpointID:     "api",
			Label:          "API",
			UpstreamScheme: "http",
			InstanceName:   "runtime-1",
			UpstreamPort:   3001,
		},
	)
	changed, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, routes)
	if err != nil {
		t.Fatalf("save runtime endpoint routes: %v", err)
	}
	if !changed {
		t.Fatalf("runtime endpoint route save was not marked changed")
	}
	statePath, err := codespaceStatePath(stateDir, codespaceUUID)
	if err != nil {
		t.Fatalf("codespace state path: %v", err)
	}
	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state codespaceState
	if err := json.Unmarshal(content, &state); err != nil {
		t.Fatalf("decode codespace state: %v", err)
	}
	if len(state.Endpoints) != 3 ||
		state.Endpoints[0].EndpointID != "api" ||
		state.Endpoints[1].EndpointID != "web" ||
		state.Endpoints[2].EndpointID != runtimeendpoint.WorkspaceEndpointID ||
		state.RuntimeMetadata.MetadataGeneration != 2 {
		t.Fatalf("state after endpoint routes = %#v", state)
	}
	changed, err = store.SaveRuntimeEndpointRoutes(codespaceUUID, nil)
	if err != nil {
		t.Fatalf("clear runtime endpoint routes: %v", err)
	}
	if !changed {
		t.Fatalf("runtime endpoint route clear was not marked changed")
	}
	content, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read cleared state: %v", err)
	}
	state = codespaceState{}
	if err := json.Unmarshal(content, &state); err != nil {
		t.Fatalf("decode cleared codespace state: %v", err)
	}
	if len(state.Endpoints) != 0 || state.RuntimeMetadata.MetadataGeneration != 3 {
		t.Fatalf("state after clearing endpoint routes = %#v", state)
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
		"state_format_version": 2,
		"endpoints": [
			{"endpoint_id": "web", "label": "Bad\u003cLabel", "upstream_scheme": "http", "upstream_host": "127.0.0.1:3000", "public": false}
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
			InstanceName:   "runtime-1",
			UpstreamPort:   3000,
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
	if err := os.WriteFile(path, []byte(`{"state_format_version":1}`), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(`{"state_format_version":1}`), 0o600); err != nil {
		t.Fatalf("write codespace state: %v", err)
	}

	service := &lockTestManagerService{}
	server := newLockTestManagerServer(t, service)
	defer server.Close()

	var output bytes.Buffer
	config := DefaultConfig()
	config.Server.ListenAddr = "127.0.0.1:0"
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
	saveManagerRegistrationForTest(t, stateDir, "https://gitea.example.com", 42)
}

func newLockTestManagerServer(t *testing.T, service *lockTestManagerService) *httptest.Server {
	t.Helper()
	return newGiteaManagerServiceServer(t, service)
}
