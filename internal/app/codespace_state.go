// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/devcontainer"
	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
)

const (
	codespaceStateFormatVersion = 3
	codespaceStateDirName       = "codespaces"
	maxCodespaceEndpoints       = devcontainer.MaxEndpointCount
)

var errEndpointLimitExceeded = errors.New("endpoint limit exceeded")

// CodespaceStateHeader stores the common format marker on a Codespace snapshot.
type CodespaceStateHeader struct {
	StateFormatVersion int `json:"state_format_version"`
}

// CodespaceStateStore reads and writes Codespace state files in state_dir.
type CodespaceStateStore struct {
	stateDir string
	sessions *gatewaySessionRegistry
	mu       sync.Mutex
}

type codespaceState struct {
	StateFormatVersion                  int                                `json:"state_format_version"`
	CodespaceUUID                       string                             `json:"codespace_uuid,omitempty"`
	RuntimeGeneration                   int64                              `json:"runtime_generation,omitempty"`
	PendingRuntimeTransition            *codespacePendingRuntimeTransition `json:"pending_runtime_transition,omitempty"`
	CleanupPending                      bool                               `json:"cleanup_pending,omitempty"`
	HealthStopPending                   bool                               `json:"health_stop_pending,omitempty"`
	HealthStopObservedOperationRVersion int64                              `json:"health_stop_observed_operation_rversion,omitempty"`
	RuntimeMetadataInactive             bool                               `json:"runtime_metadata_inactive,omitempty"`
	Endpoints                           []codespaceEndpointSnapshot        `json:"endpoints,omitempty"`
	RuntimeMetadata                     *codespaceRuntimeMetadataSnapshot  `json:"runtime_metadata,omitempty"`
	ActiveOperation                     *codespaceActiveOperation          `json:"active_operation,omitempty"`
	StartupInput                        *codespaceStartupInputSnapshot     `json:"startup_input,omitempty"`
	RuntimeEnvironment                  *provisioner.RuntimeEnvironment    `json:"runtime_environment,omitempty"`
}

type codespaceActiveOperation struct {
	OperationRVersion int64           `json:"operation_rversion"`
	WorkerStage       string          `json:"worker_stage"`
	Payload           json.RawMessage `json:"payload"`
}

type codespacePendingRuntimeTransition struct {
	TargetState               string `json:"target_state"`
	RuntimeGeneration         int64  `json:"runtime_generation"`
	ObservedOperationRVersion int64  `json:"observed_operation_rversion"`
}

type codespaceStartupInputSnapshot struct {
	RepoFullName    string                        `json:"repo_full_name"`
	Username        string                        `json:"username"`
	GitUserEmail    string                        `json:"git_user_email"`
	RuntimeUserName string                        `json:"runtime_user_name"`
	EnvironmentTag  string                        `json:"environment_tag"`
	DevContainer    codespaceDevContainerSnapshot `json:"dev_container"`
}

type codespaceDevContainerSnapshot struct {
	Source        string `json:"source"`
	Path          string `json:"path,omitempty"`
	CommitSHA     string `json:"commit_sha,omitempty"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
	DefaultImage  string `json:"default_image,omitempty"`
}

type codespaceEndpointSnapshot struct {
	EndpointID     string `json:"endpoint_id"`
	Label          string `json:"label"`
	UpstreamScheme string `json:"upstream_scheme"`
	InstanceName   string `json:"instance_name"`
	UpstreamPort   uint32 `json:"upstream_port"`
	Public         bool   `json:"public"`
}

type codespaceRuntimeMetadataSnapshot struct {
	MetadataGeneration int64                         `json:"metadata_generation"`
	InstanceName       string                        `json:"instance_name,omitempty"`
	Workdir            string                        `json:"workdir,omitempty"`
	Boot               codespaceRuntimeMetadataBoot  `json:"boot"`
	ResourceUsage      codespaceRuntimeResourceUsage `json:"resource_usage,omitempty"`
}

type codespaceRuntimeMetadataBoot struct {
	OperationRVersion int64  `json:"operation_rversion"`
	Stage             string `json:"stage"`
	StartedUnix       int64  `json:"started_unix"`
	LastUpdateUnix    int64  `json:"last_update_unix"`
}

type codespaceRuntimeResourceUsage struct {
	CPUObserved        bool  `json:"cpu_observed,omitempty"`
	CPUUsedMillicores  int64 `json:"cpu_used_millicores,omitempty"`
	CPULimitMillicores int64 `json:"cpu_limit_millicores,omitempty"`
	MemoryUsedBytes    int64 `json:"memory_used_bytes,omitempty"`
	MemoryLimitBytes   int64 `json:"memory_limit_bytes,omitempty"`
	DiskUsedBytes      int64 `json:"disk_used_bytes,omitempty"`
	DiskLimitBytes     int64 `json:"disk_limit_bytes,omitempty"`
	ObservedUnix       int64 `json:"observed_unix,omitempty"`
}

type gatewayWorkspaceTarget struct {
	instanceName     string
	workdir          string
	uid              uint32
	gid              uint32
	containerID      string
	containerUser    string
	containerWorkdir string
	editorPort       uint32
}

func (target gatewayWorkspaceTarget) commandRequest() provisioner.WorkspaceCommandRequest {
	return provisioner.WorkspaceCommandRequest{
		InstanceName: target.instanceName,
	}
}

// NewCodespaceStateStore creates a Codespace state store rooted at stateDir.
func NewCodespaceStateStore(stateDir string) *CodespaceStateStore {
	return &CodespaceStateStore{stateDir: stateDir}
}

func (s *CodespaceStateStore) SetSessionRegistry(sessions *gatewaySessionRegistry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = sessions
}

// ValidateCodespaceStateFiles checks existing Codespace snapshots before Manager starts external work.
func ValidateCodespaceStateFiles(stateDir string) error {
	dir, err := codespaceStateDir(stateDir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read codespace state dir %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("unexpected directory in codespace state dir: %s", filepath.Join(dir, entry.Name()))
		}
		if filepath.Ext(entry.Name()) != ".json" {
			return fmt.Errorf("unexpected file in codespace state dir: %s", filepath.Join(dir, entry.Name()))
		}
		codespaceUUID := strings.TrimSuffix(entry.Name(), ".json")
		if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
			return fmt.Errorf("invalid codespace state filename %s: %w", filepath.Join(dir, entry.Name()), err)
		}
		if err := validateCodespaceStateFile(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// LoadActiveOperations returns complete active operation contexts from local snapshots.
func (s *CodespaceStateStore) LoadActiveOperations() ([]manager.OperationSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := codespaceStateDir(s.stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read codespace state dir %s: %w", dir, err)
	}
	snapshots := make([]manager.OperationSnapshot, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		codespaceUUID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := loadCodespaceStateFile(filepath.Join(dir, entry.Name()), codespaceUUID)
		if err != nil {
			return nil, err
		}
		if state.CleanupPending {
			continue
		}
		if state.HealthStopPending {
			continue
		}
		if state.ActiveOperation == nil {
			continue
		}
		if state.ActiveOperation.WorkerStage == string(manager.OperationWorkerStageActive) {
			state.ActiveOperation.WorkerStage = string(manager.OperationWorkerStageLeasePaused)
			if err := writeJSONFileAtomic(filepath.Join(dir, entry.Name()), state); err != nil {
				return nil, fmt.Errorf("pause active operation state %s: %w", filepath.Join(dir, entry.Name()), err)
			}
		}
		var payload codespacev1OperationPayload
		if err := protojson.Unmarshal(state.ActiveOperation.Payload, payload.Message()); err != nil {
			return nil, fmt.Errorf("decode active operation payload %s: %w", filepath.Join(dir, entry.Name()), err)
		}
		operation := payload.OperationPayload()
		if operation.GetCodespaceUuid() != codespaceUUID {
			return nil, fmt.Errorf("active operation payload uuid %s does not match state file uuid %s", operation.GetCodespaceUuid(), codespaceUUID)
		}
		if operation.GetOperationRversion() != state.ActiveOperation.OperationRVersion {
			return nil, fmt.Errorf("active operation payload version %d does not match state version %d", operation.GetOperationRversion(), state.ActiveOperation.OperationRVersion)
		}
		if _, ok := seen[codespaceUUID]; ok {
			return nil, fmt.Errorf("duplicate codespace state %s", codespaceUUID)
		}
		seen[codespaceUUID] = struct{}{}
		snapshots = append(snapshots, manager.OperationSnapshot{
			Payload:     operation,
			WorkerStage: manager.OperationWorkerStage(state.ActiveOperation.WorkerStage),
		})
	}
	return snapshots, nil
}

// LoadRuntimeGenerations returns persisted per-Codespace runtime generation baselines.
func (s *CodespaceStateStore) LoadRuntimeGenerations() (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := codespaceStateDir(s.stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read codespace state dir %s: %w", dir, err)
	}
	generations := make(map[string]int64, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		codespaceUUID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := loadCodespaceStateFile(filepath.Join(dir, entry.Name()), codespaceUUID)
		if err != nil {
			return nil, err
		}
		if state.RuntimeGeneration > 0 {
			generations[codespaceUUID] = state.RuntimeGeneration
		}
	}
	return generations, nil
}

// LoadRuntimeTransitionPendings returns pending runtime transition reports from local snapshots.
func (s *CodespaceStateStore) LoadRuntimeTransitionPendings() ([]manager.RuntimeTransitionSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := codespaceStateDir(s.stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read codespace state dir %s: %w", dir, err)
	}
	transitions := make([]manager.RuntimeTransitionSnapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		codespaceUUID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := loadCodespaceStateFile(filepath.Join(dir, entry.Name()), codespaceUUID)
		if err != nil {
			return nil, err
		}
		if state.CleanupPending {
			continue
		}
		if state.HealthStopPending {
			continue
		}
		if state.PendingRuntimeTransition == nil {
			continue
		}
		targetState, err := runtimeTransitionTargetStateFromString(state.PendingRuntimeTransition.TargetState)
		if err != nil {
			return nil, err
		}
		transitions = append(transitions, manager.RuntimeTransitionSnapshot{
			CodespaceUUID:             codespaceUUID,
			TargetState:               targetState,
			RuntimeGeneration:         state.PendingRuntimeTransition.RuntimeGeneration,
			ObservedOperationRVersion: state.PendingRuntimeTransition.ObservedOperationRVersion,
		})
	}
	return transitions, nil
}

// LoadCleanupPendings returns Codespaces that must finish local cleanup before RPC work.
func (s *CodespaceStateStore) LoadCleanupPendings() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := codespaceStateDir(s.stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read codespace state dir %s: %w", dir, err)
	}
	codespaceUUIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		codespaceUUID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := loadCodespaceStateFile(filepath.Join(dir, entry.Name()), codespaceUUID)
		if err != nil {
			return nil, err
		}
		if state.CleanupPending {
			codespaceUUIDs = append(codespaceUUIDs, codespaceUUID)
		}
	}
	return codespaceUUIDs, nil
}

// LoadHealthStopPendings returns health-driven stops that must complete before normal RPC work.
func (s *CodespaceStateStore) LoadHealthStopPendings() ([]manager.HealthStopSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := codespaceStateDir(s.stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read codespace state dir %s: %w", dir, err)
	}
	pendings := make([]manager.HealthStopSnapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		codespaceUUID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := loadCodespaceStateFile(filepath.Join(dir, entry.Name()), codespaceUUID)
		if err != nil {
			return nil, err
		}
		if !state.HealthStopPending {
			continue
		}
		pendings = append(pendings, manager.HealthStopSnapshot{
			CodespaceUUID:             codespaceUUID,
			ObservedOperationRVersion: state.HealthStopObservedOperationRVersion,
		})
	}
	return pendings, nil
}

// LoadGatewayRoutes returns persisted Endpoint routes for Gateway startup recovery.
func (s *CodespaceStateStore) LoadGatewayRoutes() ([]gatewayEndpointRoute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := codespaceStateDir(s.stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read codespace state dir %s: %w", dir, err)
	}
	routes := make([]gatewayEndpointRoute, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		codespaceUUID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := loadCodespaceStateFile(filepath.Join(dir, entry.Name()), codespaceUUID)
		if err != nil {
			return nil, err
		}
		if state.CleanupPending || state.HealthStopPending || state.PendingRuntimeTransition != nil {
			continue
		}
		for _, endpoint := range state.Endpoints {
			routes = append(routes, gatewayEndpointRoute{
				codespaceUUID:  codespaceUUID,
				endpointID:     endpoint.EndpointID,
				label:          endpoint.Label,
				upstreamScheme: endpoint.UpstreamScheme,
				instanceName:   endpoint.InstanceName,
				upstreamPort:   endpoint.UpstreamPort,
				public:         endpoint.Public,
			})
		}
	}
	return routes, nil
}

// SaveRuntimeEndpointRoutes stores the complete trusted runtime endpoint route set.
func (s *CodespaceStateStore) SaveRuntimeEndpointRoutes(codespaceUUID string, routes []manager.RuntimeEndpointRoute) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return false, fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if len(routes) > maxCodespaceEndpoints {
		return false, errEndpointLimitExceeded
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return false, err
	}
	state, err := loadOptionalCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		return false, err
	}
	if state.RuntimeMetadataInactive && len(routes) > 0 {
		return false, fmt.Errorf("runtime metadata publication is inactive")
	}

	endpoints := make([]codespaceEndpointSnapshot, 0, len(routes))
	seen := make(map[string]struct{}, len(routes))
	workspaceFound := false
	for _, route := range routes {
		if route.CodespaceUUID == "" {
			route.CodespaceUUID = codespaceUUID
		}
		if route.CodespaceUUID != codespaceUUID {
			return false, fmt.Errorf("endpoint route codespace uuid mismatch")
		}
		localRoute, err := gatewayEndpointRouteFromManager(route)
		if err != nil {
			return false, err
		}
		if err := validateEndpointLabel(localRoute.label); err != nil {
			return false, err
		}
		if _, ok := seen[localRoute.endpointID]; ok {
			return false, fmt.Errorf("duplicate endpoint_id %s", localRoute.endpointID)
		}
		seen[localRoute.endpointID] = struct{}{}
		if localRoute.endpointID == devcontainer.WorkspaceEndpointID {
			workspaceFound = true
		}
		endpoints = append(endpoints, codespaceEndpointSnapshot{
			EndpointID:     localRoute.endpointID,
			Label:          localRoute.label,
			UpstreamScheme: localRoute.upstreamScheme,
			InstanceName:   localRoute.instanceName,
			UpstreamPort:   localRoute.upstreamPort,
			Public:         localRoute.public,
		})
	}
	if len(routes) > 0 && !workspaceFound {
		return false, fmt.Errorf("workspace endpoint route is required")
	}
	sort.Slice(endpoints, func(i, j int) bool {
		return endpoints[i].EndpointID < endpoints[j].EndpointID
	})
	if sameCodespaceEndpointSnapshots(state.Endpoints, endpoints) {
		return false, nil
	}
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = codespaceUUID
	state.Endpoints = endpoints
	if err := state.bumpRuntimeMetadataGeneration(); err != nil {
		return false, err
	}
	return true, writeCodespaceStateFile(path, state)
}

// SaveRuntimeMetadataSnapshot stores the current runtime metadata base.
func (s *CodespaceStateStore) SaveRuntimeMetadataSnapshot(snapshot manager.RuntimeMetadataSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(snapshot.CodespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if snapshot.MetadataGeneration <= 0 {
		return fmt.Errorf("metadata_generation must be positive")
	}
	if err := validateRuntimeMetadataSnapshot(snapshot); err != nil {
		return err
	}
	path, err := codespaceStatePath(s.stateDir, snapshot.CodespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadOptionalCodespaceStateFile(path, snapshot.CodespaceUUID)
	if err != nil {
		return err
	}
	oldTarget, hadOldTarget := gatewayWorkspaceTargetFromRuntimeMetadata(state.RuntimeMetadata)
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = snapshot.CodespaceUUID
	state.RuntimeMetadataInactive = false
	state.RuntimeMetadata = &codespaceRuntimeMetadataSnapshot{
		MetadataGeneration: snapshot.MetadataGeneration,
		InstanceName:       strings.TrimSpace(snapshot.InstanceName),
		Workdir:            strings.TrimSpace(snapshot.Workdir),
		Boot: codespaceRuntimeMetadataBoot{
			OperationRVersion: snapshot.Boot.OperationRVersion,
			Stage:             snapshot.Boot.Stage,
			StartedUnix:       snapshot.Boot.StartedUnix,
			LastUpdateUnix:    snapshot.Boot.LastUpdateUnix,
		},
		ResourceUsage: runtimeResourceUsageToState(snapshot.ResourceUsage),
	}
	if err := writeJSONFileAtomic(path, state); err != nil {
		return err
	}
	newTarget, hasNewTarget := gatewayWorkspaceTargetFromRuntimeMetadata(state.RuntimeMetadata)
	if s.sessions != nil && hadOldTarget && (!hasNewTarget || oldTarget != newTarget) {
		s.sessions.DeleteCodespace(snapshot.CodespaceUUID)
	}
	return nil
}

// ClearRuntimeMetadata removes the publishable runtime snapshot and its Endpoint declarations.
func (s *CodespaceStateStore) ClearRuntimeMetadata(codespaceUUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if state.RuntimeMetadataInactive && state.RuntimeMetadata == nil && len(state.Endpoints) == 0 {
		return nil
	}
	state.RuntimeMetadataInactive = true
	state.RuntimeMetadata = nil
	state.Endpoints = nil
	if err := writeCodespaceStateFile(path, state); err != nil {
		return err
	}
	if s.sessions != nil {
		s.sessions.DeleteCodespace(codespaceUUID)
	}
	return nil
}

// LoadRuntimeMetadataRequest returns the current complete typed metadata for Gitea.
func (s *CodespaceStateStore) LoadRuntimeMetadataRequest(codespaceUUID string) (int64, *codespacev1.RuntimeMetadata, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return 0, nil, false, fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return 0, nil, false, err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil, false, nil
		}
		return 0, nil, false, err
	}
	if state.RuntimeMetadata == nil {
		return 0, nil, false, nil
	}
	if state.CleanupPending || state.HealthStopPending || state.PendingRuntimeTransition != nil {
		return 0, nil, false, nil
	}
	endpoints := append([]codespaceEndpointSnapshot(nil), state.Endpoints...)
	sort.Slice(endpoints, func(i, j int) bool {
		return endpoints[i].EndpointID < endpoints[j].EndpointID
	})
	metadataEndpoints := make([]*codespacev1.RuntimeEndpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		metadataEndpoints = append(metadataEndpoints, &codespacev1.RuntimeEndpoint{
			EndpointId: endpoint.EndpointID,
			Label:      endpoint.Label,
			Public:     endpoint.Public,
		})
	}
	snapshot := manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: state.RuntimeMetadata.MetadataGeneration,
		InstanceName:       state.RuntimeMetadata.InstanceName,
		Workdir:            state.RuntimeMetadata.Workdir,
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: state.RuntimeMetadata.Boot.OperationRVersion,
			Stage:             state.RuntimeMetadata.Boot.Stage,
			StartedUnix:       state.RuntimeMetadata.Boot.StartedUnix,
			LastUpdateUnix:    state.RuntimeMetadata.Boot.LastUpdateUnix,
		},
		ResourceUsage: runtimeResourceUsageFromState(state.RuntimeMetadata.ResourceUsage),
	}
	metadata, err := manager.RuntimeMetadataProto(snapshot, metadataEndpoints)
	if err != nil {
		return 0, nil, false, fmt.Errorf("build runtime metadata: %w", err)
	}
	return state.RuntimeMetadata.MetadataGeneration, metadata, true, nil
}

func (s *CodespaceStateStore) LoadGatewayWorkspaceTarget(codespaceUUID string) (gatewayWorkspaceTarget, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return gatewayWorkspaceTarget{}, false, fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return gatewayWorkspaceTarget{}, false, err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return gatewayWorkspaceTarget{}, false, nil
		}
		return gatewayWorkspaceTarget{}, false, err
	}
	if state.CleanupPending || state.HealthStopPending || state.PendingRuntimeTransition != nil {
		return gatewayWorkspaceTarget{}, false, nil
	}
	target, ok := gatewayWorkspaceTargetFromRuntimeMetadata(state.RuntimeMetadata)
	if !ok {
		return gatewayWorkspaceTarget{}, false, nil
	}
	if state.RuntimeEnvironment == nil {
		return gatewayWorkspaceTarget{}, false, fmt.Errorf("workspace credential identity is missing")
	}
	target.uid = state.RuntimeEnvironment.User
	target.gid = state.RuntimeEnvironment.Group
	target.containerID = strings.TrimSpace(state.RuntimeEnvironment.Environment.PrimaryContainerID)
	target.containerUser = strings.TrimSpace(state.RuntimeEnvironment.Environment.RemoteUser)
	target.containerWorkdir = strings.TrimSpace(state.RuntimeEnvironment.Environment.RemoteWorkdir)
	if target.containerID == "" || target.containerUser == "" || !filepath.IsAbs(target.containerWorkdir) {
		return gatewayWorkspaceTarget{}, false, fmt.Errorf("Dev Container runtime target is missing")
	}
	editorPort := state.RuntimeEnvironment.Environment.WebIDEPort
	if editorPort != provisioner.WorkspaceIDEPort {
		return gatewayWorkspaceTarget{}, false, fmt.Errorf("Web IDE runtime target is missing")
	}
	target.editorPort = uint32(editorPort)
	return target, true, nil
}

// UpdateRuntimeResourceUsage stores the latest resource usage sample.
func (s *CodespaceStateStore) UpdateRuntimeResourceUsage(codespaceUUID string, usage provisioner.RuntimeResourceUsage) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return false, fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return false, err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if state.CleanupPending || state.HealthStopPending || state.PendingRuntimeTransition != nil || state.RuntimeMetadata == nil {
		return false, nil
	}
	next := runtimeResourceUsageToState(usage)
	if state.RuntimeMetadata.ResourceUsage == next {
		return false, nil
	}
	state.RuntimeMetadata.ResourceUsage = next
	if err := state.bumpRuntimeMetadataGeneration(); err != nil {
		return false, err
	}
	return true, writeCodespaceStateFile(path, state)
}

// LoadRuntimeMetadataSnapshot returns the persisted runtime metadata snapshot.
func (s *CodespaceStateStore) LoadRuntimeMetadataSnapshot(codespaceUUID string) (manager.RuntimeMetadataSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return manager.RuntimeMetadataSnapshot{}, false, fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return manager.RuntimeMetadataSnapshot{}, false, err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return manager.RuntimeMetadataSnapshot{}, false, nil
		}
		return manager.RuntimeMetadataSnapshot{}, false, err
	}
	if state.CleanupPending || state.HealthStopPending || state.PendingRuntimeTransition != nil || state.RuntimeMetadata == nil {
		return manager.RuntimeMetadataSnapshot{}, false, nil
	}
	return manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: state.RuntimeMetadata.MetadataGeneration,
		InstanceName:       state.RuntimeMetadata.InstanceName,
		Workdir:            state.RuntimeMetadata.Workdir,
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: state.RuntimeMetadata.Boot.OperationRVersion,
			Stage:             state.RuntimeMetadata.Boot.Stage,
			StartedUnix:       state.RuntimeMetadata.Boot.StartedUnix,
			LastUpdateUnix:    state.RuntimeMetadata.Boot.LastUpdateUnix,
		},
		ResourceUsage: runtimeResourceUsageFromState(state.RuntimeMetadata.ResourceUsage),
	}, true, nil
}

// SaveStartupInput stores the create-time startup input owned by Manager.
func (s *CodespaceStateStore) SaveStartupInput(input manager.StartupInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(input.CodespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if err := validateStartupInput(input); err != nil {
		return err
	}
	path, err := codespaceStatePath(s.stateDir, input.CodespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadOptionalCodespaceStateFile(path, input.CodespaceUUID)
	if err != nil {
		return err
	}
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = input.CodespaceUUID
	state.StartupInput = startupInputToState(input)
	return writeCodespaceStateFile(path, state)
}

// LoadStartupInput returns the persisted create-time startup input.
func (s *CodespaceStateStore) LoadStartupInput(codespaceUUID string) (manager.StartupInput, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return manager.StartupInput{}, false, fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return manager.StartupInput{}, false, err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return manager.StartupInput{}, false, nil
		}
		return manager.StartupInput{}, false, err
	}
	if state.StartupInput == nil {
		return manager.StartupInput{}, false, nil
	}
	input := startupInputFromState(codespaceUUID, state.StartupInput)
	return input, true, nil
}

func gatewayWorkspaceTargetFromRuntimeMetadata(snapshot *codespaceRuntimeMetadataSnapshot) (gatewayWorkspaceTarget, bool) {
	if snapshot == nil || snapshot.Boot.Stage != manager.RuntimeBootStageReady {
		return gatewayWorkspaceTarget{}, false
	}
	instanceName := strings.TrimSpace(snapshot.InstanceName)
	workdir := strings.TrimSpace(snapshot.Workdir)
	if instanceName == "" || workdir == "" {
		return gatewayWorkspaceTarget{}, false
	}
	return gatewayWorkspaceTarget{
		instanceName: instanceName,
		workdir:      workdir,
	}, true
}

// RebaseRuntimeMetadataGeneration moves a persisted metadata snapshot above a stale server generation.
func (s *CodespaceStateStore) RebaseRuntimeMetadataGeneration(codespaceUUID string, currentGeneration int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if currentGeneration <= 0 {
		return fmt.Errorf("current metadata generation must be positive")
	}
	if currentGeneration == math.MaxInt64 {
		return fmt.Errorf("metadata_generation is exhausted")
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		return err
	}
	if state.RuntimeMetadata == nil {
		return fmt.Errorf("runtime metadata snapshot is missing")
	}
	if state.RuntimeMetadata.MetadataGeneration > currentGeneration {
		return nil
	}
	state.RuntimeMetadata.MetadataGeneration = currentGeneration + 1
	return writeJSONFileAtomic(path, state)
}

// SaveActiveOperation stores one complete active operation context.
func (s *CodespaceStateStore) SaveActiveOperation(snapshot manager.OperationSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshot.Payload == nil {
		return fmt.Errorf("operation payload is required")
	}
	codespaceUUID := snapshot.Payload.GetCodespaceUuid()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if snapshot.Payload.GetOperationRversion() <= 0 {
		return fmt.Errorf("operation_rversion must be positive")
	}
	workerStage := snapshot.WorkerStage
	if workerStage == "" {
		workerStage = manager.OperationWorkerStageActive
	}
	if workerStage != manager.OperationWorkerStageActive && workerStage != manager.OperationWorkerStageLeasePaused {
		return fmt.Errorf("worker_stage must be active or lease_paused")
	}
	payload, err := protojson.Marshal(snapshot.Payload)
	if err != nil {
		return fmt.Errorf("encode active operation payload: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadOptionalCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		return err
	}
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = codespaceUUID
	state.ActiveOperation = &codespaceActiveOperation{
		OperationRVersion: snapshot.Payload.GetOperationRversion(),
		WorkerStage:       string(workerStage),
		Payload:           json.RawMessage(payload),
	}
	return writeJSONFileAtomic(path, state)
}

// SaveRuntimeEnvironment stores the complete local runtime target.
func (s *CodespaceStateStore) SaveRuntimeEnvironment(codespaceUUID string, environment provisioner.RuntimeEnvironment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if err := environment.Validate(); err != nil {
		return err
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadOptionalCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		return err
	}
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = codespaceUUID
	state.RuntimeEnvironment = &environment
	return writeCodespaceStateFile(path, state)
}

// LoadRuntimeEnvironment returns the complete local runtime target.
func (s *CodespaceStateStore) LoadRuntimeEnvironment(codespaceUUID string) (provisioner.RuntimeEnvironment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return provisioner.RuntimeEnvironment{}, false, fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return provisioner.RuntimeEnvironment{}, false, err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return provisioner.RuntimeEnvironment{}, false, nil
		}
		return provisioner.RuntimeEnvironment{}, false, err
	}
	if state.RuntimeEnvironment == nil {
		return provisioner.RuntimeEnvironment{}, false, nil
	}
	return *state.RuntimeEnvironment, true, nil
}

// DeleteActiveOperation clears one active operation context when it still matches the current version.
func (s *CodespaceStateStore) DeleteActiveOperation(codespaceUUID string, operationRVersion int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if operationRVersion <= 0 {
		return fmt.Errorf("operation_rversion must be positive")
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if state.ActiveOperation == nil || state.ActiveOperation.OperationRVersion != operationRVersion {
		return nil
	}
	state.ActiveOperation = nil
	if err := writeCodespaceStateFile(path, state); err != nil {
		return fmt.Errorf("clear active operation state %s: %w", path, err)
	}
	return nil
}

// SaveRuntimeTransitionPending stores a pending runtime transition before its first RPC.
func (s *CodespaceStateStore) SaveRuntimeTransitionPending(snapshot manager.RuntimeTransitionSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(snapshot.CodespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if snapshot.RuntimeGeneration <= 0 {
		return fmt.Errorf("runtime_generation must be positive")
	}
	if snapshot.ObservedOperationRVersion <= 0 {
		return fmt.Errorf("observed_operation_rversion must be positive")
	}
	targetState, err := runtimeTransitionTargetState(snapshot.TargetState)
	if err != nil {
		return err
	}
	path, err := codespaceStatePath(s.stateDir, snapshot.CodespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadOptionalCodespaceStateFile(path, snapshot.CodespaceUUID)
	if err != nil {
		return err
	}
	if snapshot.RuntimeGeneration <= state.RuntimeGeneration {
		return fmt.Errorf("runtime_generation must be greater than current value %d", state.RuntimeGeneration)
	}
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = snapshot.CodespaceUUID
	state.RuntimeGeneration = snapshot.RuntimeGeneration
	state.HealthStopPending = false
	state.HealthStopObservedOperationRVersion = 0
	state.PendingRuntimeTransition = &codespacePendingRuntimeTransition{
		TargetState:               targetState,
		RuntimeGeneration:         snapshot.RuntimeGeneration,
		ObservedOperationRVersion: snapshot.ObservedOperationRVersion,
	}
	return writeJSONFileAtomic(path, state)
}

// ClearRuntimeTransitionPending clears the pending transition after the matching report is resolved.
func (s *CodespaceStateStore) ClearRuntimeTransitionPending(codespaceUUID string, runtimeGeneration int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if runtimeGeneration <= 0 {
		return fmt.Errorf("runtime_generation must be positive")
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if state.PendingRuntimeTransition == nil || state.PendingRuntimeTransition.RuntimeGeneration != runtimeGeneration {
		return nil
	}
	state.PendingRuntimeTransition = nil
	return writeCodespaceStateFile(path, state)
}

// SaveHealthStopPending stores a health-driven stop intent before stopping runtime resources.
func (s *CodespaceStateStore) SaveHealthStopPending(snapshot manager.HealthStopSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(snapshot.CodespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	if snapshot.ObservedOperationRVersion <= 0 {
		return fmt.Errorf("observed_operation_rversion must be positive")
	}
	path, err := codespaceStatePath(s.stateDir, snapshot.CodespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadOptionalCodespaceStateFile(path, snapshot.CodespaceUUID)
	if err != nil {
		return err
	}
	if state.CleanupPending || state.PendingRuntimeTransition != nil {
		return fmt.Errorf("health_stop_pending cannot coexist with cleanup_pending or pending_runtime_transition")
	}
	if state.RuntimeMetadata == nil ||
		state.RuntimeMetadata.Boot.Stage != manager.RuntimeBootStageReady ||
		state.RuntimeMetadata.Boot.OperationRVersion != snapshot.ObservedOperationRVersion {
		return fmt.Errorf("health_stop_pending requires matching ready runtime metadata")
	}
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = snapshot.CodespaceUUID
	state.HealthStopPending = true
	state.HealthStopObservedOperationRVersion = snapshot.ObservedOperationRVersion
	return writeJSONFileAtomic(path, state)
}

// SaveCleanupPending stores the local cleanup state before deleting runtime resources.
func (s *CodespaceStateStore) SaveCleanupPending(codespaceUUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadOptionalCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		return err
	}
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = codespaceUUID
	state.PendingRuntimeTransition = nil
	state.HealthStopPending = false
	state.HealthStopObservedOperationRVersion = 0
	state.CleanupPending = true
	return writeJSONFileAtomic(path, state)
}

// ClearCodespaceState removes the local Codespace snapshot after cleanup completes.
func (s *CodespaceStateStore) ClearCodespaceState(codespaceUUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove codespace state %s: %w", path, err)
	}
	return syncStateDir(filepath.Dir(path))
}

func validateCodespaceStateFile(path string) error {
	_, err := loadCodespaceStateFile(path, strings.TrimSuffix(filepath.Base(path), ".json"))
	return err
}

func writeCodespaceStateFile(path string, state codespaceState) error {
	if state.hasPersistentData() {
		return writeJSONFileAtomic(path, state)
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove codespace state %s: %w", path, err)
	}
	return syncStateDir(filepath.Dir(path))
}

func loadCodespaceStateFile(path string, codespaceUUID string) (codespaceState, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return codespaceState{}, fmt.Errorf("read codespace state %s: %w", path, err)
	}
	var state codespaceState
	if err := json.Unmarshal(content, &state); err != nil {
		return codespaceState{}, fmt.Errorf("decode codespace state %s: %w", path, err)
	}
	if state.StateFormatVersion != codespaceStateFormatVersion {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: state_format_version must be %d", path, codespaceStateFormatVersion)
	}
	if state.CodespaceUUID != "" && state.CodespaceUUID != codespaceUUID {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: codespace_uuid must match filename", path)
	}
	if state.RuntimeGeneration < 0 {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: runtime_generation must not be negative", path)
	}
	if err := validatePendingRuntimeTransitionState(path, state); err != nil {
		return codespaceState{}, err
	}
	if state.CleanupPending && state.PendingRuntimeTransition != nil {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: cleanup_pending cannot coexist with pending_runtime_transition", path)
	}
	if state.HealthStopPending && (state.CleanupPending || state.PendingRuntimeTransition != nil) {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: health_stop_pending cannot coexist with cleanup_pending or pending_runtime_transition", path)
	}
	if state.HealthStopPending {
		if state.HealthStopObservedOperationRVersion <= 0 {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: health_stop_observed_operation_rversion must be positive", path)
		}
	} else if state.HealthStopObservedOperationRVersion != 0 {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: health_stop_observed_operation_rversion requires health_stop_pending", path)
	}
	if state.RuntimeMetadataInactive && (state.RuntimeMetadata != nil || len(state.Endpoints) > 0) {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: inactive runtime metadata cannot contain metadata or endpoints", path)
	}
	if len(state.Endpoints) > maxCodespaceEndpoints {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: endpoints exceed limit %d", path, maxCodespaceEndpoints)
	}
	if err := validateStoredRuntimeMetadataState(path, state.RuntimeMetadata); err != nil {
		return codespaceState{}, err
	}
	if err := validateStoredEndpointRoutes(path, codespaceUUID, state.Endpoints); err != nil {
		return codespaceState{}, err
	}
	if err := validateActiveOperationState(path, state.ActiveOperation); err != nil {
		return codespaceState{}, err
	}
	if err := validateStartupInputState(path, codespaceUUID, state.StartupInput); err != nil {
		return codespaceState{}, err
	}
	if state.RuntimeEnvironment != nil {
		if err := state.RuntimeEnvironment.Validate(); err != nil {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: %w", path, err)
		}
	}
	return state, nil
}

func validatePendingRuntimeTransitionState(path string, state codespaceState) error {
	if state.PendingRuntimeTransition == nil {
		return nil
	}
	if _, err := runtimeTransitionTargetStateFromString(state.PendingRuntimeTransition.TargetState); err != nil {
		return fmt.Errorf("validate codespace state %s: %w", path, err)
	}
	if state.PendingRuntimeTransition.RuntimeGeneration <= 0 {
		return fmt.Errorf("validate codespace state %s: pending runtime_generation must be positive", path)
	}
	if state.PendingRuntimeTransition.ObservedOperationRVersion <= 0 {
		return fmt.Errorf("validate codespace state %s: pending observed_operation_rversion must be positive", path)
	}
	if state.PendingRuntimeTransition.RuntimeGeneration > state.RuntimeGeneration {
		return fmt.Errorf("validate codespace state %s: pending runtime_generation exceeds current runtime_generation", path)
	}
	return nil
}

func validateStoredRuntimeMetadataState(path string, metadata *codespaceRuntimeMetadataSnapshot) error {
	if metadata == nil {
		return nil
	}
	if metadata.MetadataGeneration <= 0 {
		return fmt.Errorf("validate codespace state %s: metadata_generation must be positive", path)
	}
	if err := validateRuntimeMetadataState(*metadata); err != nil {
		return fmt.Errorf("validate codespace state %s: %w", path, err)
	}
	return nil
}

func validateStoredEndpointRoutes(path string, codespaceUUID string, endpoints []codespaceEndpointSnapshot) error {
	seenEndpoints := make(map[string]struct{}, len(endpoints))
	workspaceFound := false
	for _, endpoint := range endpoints {
		route, err := normalizeGatewayEndpointRoute(gatewayEndpointRoute{
			codespaceUUID:  codespaceUUID,
			endpointID:     endpoint.EndpointID,
			label:          endpoint.Label,
			upstreamScheme: endpoint.UpstreamScheme,
			instanceName:   endpoint.InstanceName,
			upstreamPort:   endpoint.UpstreamPort,
			public:         endpoint.Public,
		})
		if err != nil {
			return fmt.Errorf("validate codespace state %s: %w", path, err)
		}
		if _, ok := seenEndpoints[route.endpointID]; ok {
			return fmt.Errorf("validate codespace state %s: duplicate endpoint_id %s", path, route.endpointID)
		}
		seenEndpoints[route.endpointID] = struct{}{}
		if route.endpointID == devcontainer.WorkspaceEndpointID {
			workspaceFound = true
		}
		if err := validateEndpointLabel(route.label); err != nil {
			return fmt.Errorf("validate codespace state %s: %w", path, err)
		}
	}
	if len(endpoints) > 0 && !workspaceFound {
		return fmt.Errorf("validate codespace state %s: workspace endpoint route is required", path)
	}
	return nil
}

func validateActiveOperationState(path string, operation *codespaceActiveOperation) error {
	if operation == nil {
		return nil
	}
	if operation.OperationRVersion <= 0 {
		return fmt.Errorf("validate codespace state %s: active operation_rversion must be positive", path)
	}
	switch operation.WorkerStage {
	case "":
		operation.WorkerStage = string(manager.OperationWorkerStageActive)
	case string(manager.OperationWorkerStageActive), string(manager.OperationWorkerStageLeasePaused):
	default:
		return fmt.Errorf("validate codespace state %s: active operation worker_stage is invalid", path)
	}
	if len(operation.Payload) == 0 {
		return fmt.Errorf("validate codespace state %s: active operation payload is required", path)
	}
	return nil
}

func startupInputToState(input manager.StartupInput) *codespaceStartupInputSnapshot {
	return &codespaceStartupInputSnapshot{
		RepoFullName:    strings.TrimSpace(input.RepoFullName),
		Username:        strings.TrimSpace(input.Username),
		GitUserEmail:    strings.TrimSpace(input.GitUserEmail),
		RuntimeUserName: strings.TrimSpace(input.RuntimeUserName),
		EnvironmentTag:  strings.TrimSpace(input.EnvironmentTag),
		DevContainer: codespaceDevContainerSnapshot{
			Source:        strings.TrimSpace(input.DevContainer.Source),
			Path:          strings.TrimSpace(input.DevContainer.Path),
			CommitSHA:     strings.TrimSpace(input.DevContainer.CommitSHA),
			ContentSHA256: strings.TrimSpace(input.DevContainer.ContentSHA256),
			DefaultImage:  strings.TrimSpace(input.DevContainer.DefaultImage),
		},
	}
}

func startupInputFromState(codespaceUUID string, snapshot *codespaceStartupInputSnapshot) manager.StartupInput {
	return manager.StartupInput{
		CodespaceUUID:   codespaceUUID,
		RepoFullName:    strings.TrimSpace(snapshot.RepoFullName),
		Username:        strings.TrimSpace(snapshot.Username),
		GitUserEmail:    strings.TrimSpace(snapshot.GitUserEmail),
		RuntimeUserName: strings.TrimSpace(snapshot.RuntimeUserName),
		EnvironmentTag:  strings.TrimSpace(snapshot.EnvironmentTag),
		DevContainer: provisioner.DevContainerConfiguration{
			Source:        strings.TrimSpace(snapshot.DevContainer.Source),
			Path:          strings.TrimSpace(snapshot.DevContainer.Path),
			CommitSHA:     strings.TrimSpace(snapshot.DevContainer.CommitSHA),
			ContentSHA256: strings.TrimSpace(snapshot.DevContainer.ContentSHA256),
			DefaultImage:  strings.TrimSpace(snapshot.DevContainer.DefaultImage),
		},
	}
}

func validateStartupInput(input manager.StartupInput) error {
	if strings.TrimSpace(input.RepoFullName) == "" {
		return fmt.Errorf("startup input repository full name is required")
	}
	if strings.TrimSpace(input.Username) == "" {
		return fmt.Errorf("startup input username is required")
	}
	if strings.TrimSpace(input.GitUserEmail) == "" {
		return fmt.Errorf("startup input git user email is required")
	}
	if strings.TrimSpace(input.RuntimeUserName) == "" {
		return fmt.Errorf("startup input runtime user name is required")
	}
	if strings.TrimSpace(input.EnvironmentTag) == "" {
		return fmt.Errorf("startup input environment tag is required")
	}
	if err := input.DevContainer.Validate(); err != nil {
		return fmt.Errorf("startup input Dev Container configuration is invalid: %w", err)
	}
	return nil
}

func validateStartupInputState(path, codespaceUUID string, snapshot *codespaceStartupInputSnapshot) error {
	if snapshot == nil {
		return nil
	}
	input := startupInputFromState(codespaceUUID, snapshot)
	if err := validateStartupInput(input); err != nil {
		return fmt.Errorf("validate codespace state %s: %w", path, err)
	}
	return nil
}

func loadOptionalCodespaceStateFile(path string, codespaceUUID string) (codespaceState, error) {
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return codespaceState{
				StateFormatVersion: codespaceStateFormatVersion,
				CodespaceUUID:      codespaceUUID,
			}, nil
		}
		return codespaceState{}, err
	}
	return state, nil
}

func (s codespaceState) hasPersistentData() bool {
	return s.RuntimeGeneration > 0 || s.PendingRuntimeTransition != nil || s.CleanupPending || s.HealthStopPending || s.RuntimeMetadataInactive || len(s.Endpoints) > 0 || s.RuntimeMetadata != nil || s.ActiveOperation != nil || s.StartupInput != nil || s.RuntimeEnvironment != nil
}

func (s *codespaceState) bumpRuntimeMetadataGeneration() error {
	if s.RuntimeMetadata == nil {
		return nil
	}
	if s.RuntimeMetadata.MetadataGeneration == math.MaxInt64 {
		return fmt.Errorf("metadata_generation is exhausted")
	}
	s.RuntimeMetadata.MetadataGeneration++
	return nil
}

func sameCodespaceEndpointSnapshot(left, right codespaceEndpointSnapshot) bool {
	return left.EndpointID == right.EndpointID &&
		left.Label == right.Label &&
		left.UpstreamScheme == right.UpstreamScheme &&
		left.InstanceName == right.InstanceName &&
		left.UpstreamPort == right.UpstreamPort &&
		left.Public == right.Public
}

func sameCodespaceEndpointSnapshots(left, right []codespaceEndpointSnapshot) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]codespaceEndpointSnapshot(nil), left...)
	rightCopy := append([]codespaceEndpointSnapshot(nil), right...)
	sort.Slice(leftCopy, func(i, j int) bool {
		return leftCopy[i].EndpointID < leftCopy[j].EndpointID
	})
	sort.Slice(rightCopy, func(i, j int) bool {
		return rightCopy[i].EndpointID < rightCopy[j].EndpointID
	})
	for i := range leftCopy {
		if !sameCodespaceEndpointSnapshot(leftCopy[i], rightCopy[i]) {
			return false
		}
	}
	return true
}

func runtimeResourceUsageToState(usage provisioner.RuntimeResourceUsage) codespaceRuntimeResourceUsage {
	return codespaceRuntimeResourceUsage{
		CPUObserved:        usage.CPUObserved,
		CPUUsedMillicores:  usage.CPUUsedMillicores,
		CPULimitMillicores: usage.CPULimitMillicores,
		MemoryUsedBytes:    usage.MemoryUsedBytes,
		MemoryLimitBytes:   usage.MemoryLimitBytes,
		DiskUsedBytes:      usage.DiskUsedBytes,
		DiskLimitBytes:     usage.DiskLimitBytes,
		ObservedUnix:       usage.ObservedUnix,
	}
}

func runtimeResourceUsageFromState(usage codespaceRuntimeResourceUsage) provisioner.RuntimeResourceUsage {
	return provisioner.RuntimeResourceUsage{
		CPUObserved:        usage.CPUObserved,
		CPUUsedMillicores:  usage.CPUUsedMillicores,
		CPULimitMillicores: usage.CPULimitMillicores,
		MemoryUsedBytes:    usage.MemoryUsedBytes,
		MemoryLimitBytes:   usage.MemoryLimitBytes,
		DiskUsedBytes:      usage.DiskUsedBytes,
		DiskLimitBytes:     usage.DiskLimitBytes,
		ObservedUnix:       usage.ObservedUnix,
	}
}

func validateRuntimeMetadataSnapshot(snapshot manager.RuntimeMetadataSnapshot) error {
	return validateRuntimeMetadataState(codespaceRuntimeMetadataSnapshot{
		MetadataGeneration: snapshot.MetadataGeneration,
		InstanceName:       strings.TrimSpace(snapshot.InstanceName),
		Workdir:            strings.TrimSpace(snapshot.Workdir),
		Boot: codespaceRuntimeMetadataBoot{
			OperationRVersion: snapshot.Boot.OperationRVersion,
			Stage:             snapshot.Boot.Stage,
			StartedUnix:       snapshot.Boot.StartedUnix,
			LastUpdateUnix:    snapshot.Boot.LastUpdateUnix,
		},
		ResourceUsage: runtimeResourceUsageToState(snapshot.ResourceUsage),
	})
}

func validateRuntimeMetadataState(snapshot codespaceRuntimeMetadataSnapshot) error {
	if snapshot.MetadataGeneration <= 0 {
		return fmt.Errorf("metadata_generation must be positive")
	}
	if snapshot.Boot.OperationRVersion <= 0 {
		return fmt.Errorf("boot operation_rversion must be positive")
	}
	if !manager.IsRuntimeBootStage(snapshot.Boot.Stage) {
		return fmt.Errorf("boot stage is invalid")
	}
	if snapshot.Boot.StartedUnix <= 0 {
		return fmt.Errorf("boot started_unix must be positive")
	}
	if snapshot.Boot.LastUpdateUnix < snapshot.Boot.StartedUnix {
		return fmt.Errorf("boot last_update_unix must be greater than or equal to started_unix")
	}
	if err := validateRuntimeResourceUsage(snapshot.ResourceUsage); err != nil {
		return err
	}
	return nil
}

func validateRuntimeResourceUsage(usage codespaceRuntimeResourceUsage) error {
	if usage.CPUUsedMillicores < 0 || usage.CPULimitMillicores < 0 {
		return fmt.Errorf("runtime cpu usage must be non-negative")
	}
	if usage.MemoryUsedBytes < 0 || usage.MemoryLimitBytes < 0 {
		return fmt.Errorf("runtime memory usage must be non-negative")
	}
	if usage.DiskUsedBytes < 0 || usage.DiskLimitBytes < 0 {
		return fmt.Errorf("runtime disk usage must be non-negative")
	}
	if usage.ObservedUnix < 0 {
		return fmt.Errorf("runtime usage observed_unix must be non-negative")
	}
	return nil
}

func validateEndpointLabel(label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("endpoint label is required")
	}
	if !utf8.ValidString(label) {
		return fmt.Errorf("endpoint label must be valid UTF-8")
	}
	if utf8.RuneCountInString(label) > 64 {
		return fmt.Errorf("endpoint label is too long")
	}
	for _, r := range label {
		if unicode.IsControl(r) || r == '<' || r == '>' {
			return fmt.Errorf("endpoint label contains an invalid character")
		}
	}
	return nil
}

func runtimeTransitionTargetState(state codespacev1.RuntimeState) (string, error) {
	switch state {
	case codespacev1.RuntimeState_RUNTIME_STATE_STOPPED:
		return "stopped", nil
	case codespacev1.RuntimeState_RUNTIME_STATE_FAILED:
		return "failed", nil
	default:
		return "", fmt.Errorf("target_state must be stopped or failed")
	}
}

func runtimeTransitionTargetStateFromString(state string) (codespacev1.RuntimeState, error) {
	switch state {
	case "stopped":
		return codespacev1.RuntimeState_RUNTIME_STATE_STOPPED, nil
	case "failed":
		return codespacev1.RuntimeState_RUNTIME_STATE_FAILED, nil
	default:
		return codespacev1.RuntimeState_RUNTIME_STATE_UNSPECIFIED, fmt.Errorf("pending target_state must be stopped or failed")
	}
}

func codespaceStateDir(stateDir string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return "", fmt.Errorf("manager.state_dir is required")
	}
	return filepath.Join(stateDir, codespaceStateDirName), nil
}

func codespaceStatePath(stateDir string, codespaceUUID string) (string, error) {
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return "", err
	}
	dir, err := codespaceStateDir(stateDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, codespaceUUID+".json"), nil
}

func validateCodespaceStateUUID(codespaceUUID string) error {
	parsed, err := uuid.Parse(codespaceUUID)
	if err != nil {
		return err
	}
	if parsed.Version() != 4 || parsed.String() != codespaceUUID {
		return fmt.Errorf("codespace uuid must be canonical lower-case UUID v4")
	}
	return nil
}

type codespacev1OperationPayload struct {
	payload codespacev1.OperationPayload
}

func (p *codespacev1OperationPayload) Message() *codespacev1.OperationPayload {
	return &p.payload
}

func (p *codespacev1OperationPayload) OperationPayload() *codespacev1.OperationPayload {
	return &p.payload
}
