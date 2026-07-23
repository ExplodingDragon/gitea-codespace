// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
)

const (
	codespaceStateFormatVersion = 1
	codespaceStateDirName       = "codespaces"
	maxCodespaceEndpoints       = 64
)

var errEndpointLimitExceeded = errors.New("endpoint limit exceeded")

// CodespaceStateHeader stores the common format marker on a Codespace snapshot.
type CodespaceStateHeader struct {
	StateFormatVersion int `json:"state_format_version"`
}

// CodespaceStateStore reads and writes Codespace state files in state_dir.
type CodespaceStateStore struct {
	stateDir string
}

type codespaceState struct {
	StateFormatVersion       int                                `json:"state_format_version"`
	CodespaceUUID            string                             `json:"codespace_uuid,omitempty"`
	RuntimeGeneration        int64                              `json:"runtime_generation,omitempty"`
	RuntimeTokenHash         string                             `json:"runtime_token_sha256,omitempty"`
	PendingRuntimeTransition *codespacePendingRuntimeTransition `json:"pending_runtime_transition,omitempty"`
	CleanupPending           bool                               `json:"cleanup_pending,omitempty"`
	Endpoints                []codespaceEndpointSnapshot        `json:"endpoints,omitempty"`
	RuntimeMetadata          *codespaceRuntimeMetadataSnapshot  `json:"runtime_metadata,omitempty"`
	ActiveOperation          *codespaceActiveOperation          `json:"active_operation,omitempty"`
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

type codespaceEndpointSnapshot struct {
	EndpointID     string `json:"endpoint_id"`
	Label          string `json:"label"`
	UpstreamScheme string `json:"upstream_scheme"`
	UpstreamHost   string `json:"upstream_host"`
	Public         bool   `json:"public"`
}

type codespaceRuntimeMetadataSnapshot struct {
	MetadataGeneration int64                        `json:"metadata_generation"`
	InternalSSH        codespaceRuntimeMetadataSSH  `json:"internal_ssh"`
	Boot               codespaceRuntimeMetadataBoot `json:"boot"`
}

type codespaceRuntimeMetadataSSH struct {
	Host               string `json:"host"`
	Port               int    `json:"port"`
	User               string `json:"user"`
	AuthMode           string `json:"auth_mode"`
	HostKeyFingerprint string `json:"host_key_fingerprint"`
}

type codespaceRuntimeMetadataBoot struct {
	OperationRVersion int64  `json:"operation_rversion"`
	Stage             string `json:"stage"`
	StartedUnix       int64  `json:"started_unix"`
	LastUpdateUnix    int64  `json:"last_update_unix"`
}

type runtimeAPIOperationType string

const (
	runtimeAPIOperationNone   runtimeAPIOperationType = ""
	runtimeAPIOperationCreate runtimeAPIOperationType = "create"
	runtimeAPIOperationResume runtimeAPIOperationType = "resume"
	runtimeAPIOperationStop   runtimeAPIOperationType = "stop"
	runtimeAPIOperationDelete runtimeAPIOperationType = "delete"
)

// NewCodespaceStateStore creates a Codespace state store rooted at stateDir.
func NewCodespaceStateStore(stateDir string) *CodespaceStateStore {
	return &CodespaceStateStore{stateDir: stateDir}
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

// LoadGatewayRoutes returns persisted Endpoint routes for Gateway startup recovery.
func (s *CodespaceStateStore) LoadGatewayRoutes() ([]gatewayEndpointRoute, error) {
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
		if state.CleanupPending {
			continue
		}
		for _, endpoint := range state.Endpoints {
			routes = append(routes, gatewayEndpointRoute{
				codespaceUUID:  codespaceUUID,
				endpointID:     endpoint.EndpointID,
				label:          endpoint.Label,
				upstreamScheme: endpoint.UpstreamScheme,
				upstreamHost:   endpoint.UpstreamHost,
				public:         endpoint.Public,
			})
		}
	}
	return routes, nil
}

// SaveEndpointRoute stores one Endpoint route in the local Codespace snapshot.
func (s *CodespaceStateStore) SaveEndpointRoute(route gatewayEndpointRoute) error {
	route, err := normalizeGatewayEndpointRoute(route)
	if err != nil {
		return err
	}
	if err := validateEndpointLabel(route.label); err != nil {
		return err
	}
	path, err := codespaceStatePath(s.stateDir, route.codespaceUUID)
	if err != nil {
		return err
	}
	state, err := loadOptionalCodespaceStateFile(path, route.codespaceUUID)
	if err != nil {
		return err
	}
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = route.codespaceUUID
	endpoint := codespaceEndpointSnapshot{
		EndpointID:     route.endpointID,
		Label:          route.label,
		UpstreamScheme: route.upstreamScheme,
		UpstreamHost:   route.upstreamHost,
		Public:         route.public,
	}
	for i := range state.Endpoints {
		if state.Endpoints[i].EndpointID == endpoint.EndpointID {
			if sameCodespaceEndpointSnapshot(state.Endpoints[i], endpoint) {
				return writeJSONFileAtomic(path, state)
			}
			state.Endpoints[i] = endpoint
			if err := state.bumpRuntimeMetadataGeneration(); err != nil {
				return err
			}
			return writeJSONFileAtomic(path, state)
		}
	}
	if len(state.Endpoints) >= maxCodespaceEndpoints {
		return errEndpointLimitExceeded
	}
	state.Endpoints = append(state.Endpoints, endpoint)
	if err := state.bumpRuntimeMetadataGeneration(); err != nil {
		return err
	}
	return writeJSONFileAtomic(path, state)
}

// DeleteEndpointRoute removes one Endpoint route from the local Codespace snapshot.
func (s *CodespaceStateStore) DeleteEndpointRoute(codespaceUUID, endpointID string) error {
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	endpointID = strings.TrimSpace(endpointID)
	if endpointID != "workspace" && !isGatewayEndpointID(endpointID) {
		return fmt.Errorf("endpoint_id is invalid")
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
	kept := state.Endpoints[:0]
	for _, endpoint := range state.Endpoints {
		if endpoint.EndpointID != endpointID {
			kept = append(kept, endpoint)
		}
	}
	if len(kept) == len(state.Endpoints) {
		return nil
	}
	state.Endpoints = kept
	if err := state.bumpRuntimeMetadataGeneration(); err != nil {
		return err
	}
	if !state.hasPersistentData() {
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("remove codespace state %s: %w", path, err)
		}
		return syncStateDir(filepath.Dir(path))
	}
	return writeJSONFileAtomic(path, state)
}

// SaveRuntimeMetadataSnapshot stores the current ready runtime metadata base.
func (s *CodespaceStateStore) SaveRuntimeMetadataSnapshot(snapshot manager.RuntimeMetadataSnapshot) error {
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
	state.StateFormatVersion = codespaceStateFormatVersion
	state.CodespaceUUID = snapshot.CodespaceUUID
	state.RuntimeMetadata = &codespaceRuntimeMetadataSnapshot{
		MetadataGeneration: snapshot.MetadataGeneration,
		InternalSSH: codespaceRuntimeMetadataSSH{
			Host:               snapshot.InternalSSH.Host,
			Port:               snapshot.InternalSSH.Port,
			User:               snapshot.InternalSSH.User,
			AuthMode:           snapshot.InternalSSH.AuthMode,
			HostKeyFingerprint: snapshot.InternalSSH.HostKeyFingerprint,
		},
		Boot: codespaceRuntimeMetadataBoot{
			OperationRVersion: snapshot.Boot.OperationRVersion,
			Stage:             snapshot.Boot.Stage,
			StartedUnix:       snapshot.Boot.StartedUnix,
			LastUpdateUnix:    snapshot.Boot.LastUpdateUnix,
		},
	}
	return writeJSONFileAtomic(path, state)
}

// LoadRuntimeMetadataRequest returns the current complete metadata JSON for Gitea.
func (s *CodespaceStateStore) LoadRuntimeMetadataRequest(codespaceUUID string) (int64, string, bool, error) {
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return 0, "", false, fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return 0, "", false, err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, "", false, nil
		}
		return 0, "", false, err
	}
	if state.RuntimeMetadata == nil {
		return 0, "", false, nil
	}
	endpoints := append([]codespaceEndpointSnapshot(nil), state.Endpoints...)
	sort.Slice(endpoints, func(i, j int) bool {
		return endpoints[i].EndpointID < endpoints[j].EndpointID
	})
	metadataEndpoints := make([]map[string]any, 0, len(endpoints))
	for _, endpoint := range endpoints {
		metadataEndpoints = append(metadataEndpoints, map[string]any{
			"endpoint_id": endpoint.EndpointID,
			"label":       endpoint.Label,
			"public":      endpoint.Public,
		})
	}
	metadata := map[string]any{
		"runtime": map[string]any{
			"internal_ssh": map[string]any{
				"host":                 state.RuntimeMetadata.InternalSSH.Host,
				"port":                 state.RuntimeMetadata.InternalSSH.Port,
				"user":                 state.RuntimeMetadata.InternalSSH.User,
				"auth_mode":            state.RuntimeMetadata.InternalSSH.AuthMode,
				"host_key_fingerprint": state.RuntimeMetadata.InternalSSH.HostKeyFingerprint,
			},
		},
		"endpoints": metadataEndpoints,
		"boot": map[string]any{
			"operation_rversion": state.RuntimeMetadata.Boot.OperationRVersion,
			"stage":              state.RuntimeMetadata.Boot.Stage,
			"started_unix":       state.RuntimeMetadata.Boot.StartedUnix,
			"last_update_unix":   state.RuntimeMetadata.Boot.LastUpdateUnix,
		},
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return 0, "", false, fmt.Errorf("encode runtime metadata: %w", err)
	}
	return state.RuntimeMetadata.MetadataGeneration, string(encoded), true, nil
}

// RuntimeAPIOperation returns the current active operation type relevant to the Runtime API.
func (s *CodespaceStateStore) RuntimeAPIOperation(codespaceUUID string) (runtimeAPIOperationType, error) {
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return runtimeAPIOperationNone, fmt.Errorf("invalid codespace uuid: %w", err)
	}
	path, err := codespaceStatePath(s.stateDir, codespaceUUID)
	if err != nil {
		return runtimeAPIOperationNone, err
	}
	state, err := loadCodespaceStateFile(path, codespaceUUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runtimeAPIOperationNone, nil
		}
		return runtimeAPIOperationNone, err
	}
	if state.CleanupPending || state.PendingRuntimeTransition != nil {
		return runtimeAPIOperationDelete, nil
	}
	if state.ActiveOperation == nil {
		return runtimeAPIOperationNone, nil
	}
	var payload codespacev1OperationPayload
	if err := protojson.Unmarshal(state.ActiveOperation.Payload, payload.Message()); err != nil {
		return runtimeAPIOperationNone, fmt.Errorf("decode active operation payload: %w", err)
	}
	operation := payload.OperationPayload()
	switch {
	case operation.GetCreate() != nil:
		return runtimeAPIOperationCreate, nil
	case operation.GetResume() != nil:
		return runtimeAPIOperationResume, nil
	case operation.GetStop() != nil:
		return runtimeAPIOperationStop, nil
	case operation.GetDelete() != nil:
		return runtimeAPIOperationDelete, nil
	default:
		return runtimeAPIOperationNone, nil
	}
}

// SaveRuntimeCredential stores the verifier for the current Runtime API token.
func (s *CodespaceStateStore) SaveRuntimeCredential(codespaceUUID, token string) error {
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return fmt.Errorf("invalid codespace uuid: %w", err)
	}
	tokenHash, err := runtimeTokenHash(token)
	if err != nil {
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
	state.RuntimeTokenHash = tokenHash
	return writeJSONFileAtomic(path, state)
}

// ResolveRuntimeToken returns the Codespace UUID bound to a Runtime API bearer token.
func (s *CodespaceStateStore) ResolveRuntimeToken(token string) (string, bool, error) {
	tokenHash, err := runtimeTokenHash(token)
	if err != nil {
		return "", false, nil
	}
	dir, err := codespaceStateDir(s.stateDir)
	if err != nil {
		return "", false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read codespace state dir %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		codespaceUUID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := loadCodespaceStateFile(filepath.Join(dir, entry.Name()), codespaceUUID)
		if err != nil {
			return "", false, err
		}
		if state.RuntimeTokenHash == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(state.RuntimeTokenHash), []byte(tokenHash)) == 1 {
			return codespaceUUID, true, nil
		}
	}
	return "", false, nil
}

// SaveActiveOperation stores one complete active operation context.
func (s *CodespaceStateStore) SaveActiveOperation(snapshot manager.OperationSnapshot) error {
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

// DeleteActiveOperation clears one active operation context when it still matches the current version.
func (s *CodespaceStateStore) DeleteActiveOperation(codespaceUUID string, operationRVersion int64) error {
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
	if !state.hasPersistentData() {
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("remove codespace state %s: %w", path, err)
		}
		return syncStateDir(filepath.Dir(path))
	}
	if err := writeJSONFileAtomic(path, state); err != nil {
		return fmt.Errorf("clear active operation state %s: %w", path, err)
	}
	return nil
}

// SaveRuntimeTransitionPending stores a pending runtime transition before its first RPC.
func (s *CodespaceStateStore) SaveRuntimeTransitionPending(snapshot manager.RuntimeTransitionSnapshot) error {
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
	state.PendingRuntimeTransition = &codespacePendingRuntimeTransition{
		TargetState:               targetState,
		RuntimeGeneration:         snapshot.RuntimeGeneration,
		ObservedOperationRVersion: snapshot.ObservedOperationRVersion,
	}
	return writeJSONFileAtomic(path, state)
}

// ClearRuntimeTransitionPending clears the pending transition after the matching report is resolved.
func (s *CodespaceStateStore) ClearRuntimeTransitionPending(codespaceUUID string, runtimeGeneration int64) error {
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
	return writeJSONFileAtomic(path, state)
}

// SaveCleanupPending stores the local cleanup state before deleting runtime resources.
func (s *CodespaceStateStore) SaveCleanupPending(codespaceUUID string) error {
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
	state.CleanupPending = true
	return writeJSONFileAtomic(path, state)
}

// ClearCodespaceState removes the local Codespace snapshot after cleanup completes.
func (s *CodespaceStateStore) ClearCodespaceState(codespaceUUID string) error {
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
	if state.RuntimeTokenHash != "" && !isRuntimeTokenHash(state.RuntimeTokenHash) {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: runtime_token_sha256 is invalid", path)
	}
	if state.PendingRuntimeTransition != nil {
		if _, err := runtimeTransitionTargetStateFromString(state.PendingRuntimeTransition.TargetState); err != nil {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: %w", path, err)
		}
		if state.PendingRuntimeTransition.RuntimeGeneration <= 0 {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: pending runtime_generation must be positive", path)
		}
		if state.PendingRuntimeTransition.ObservedOperationRVersion <= 0 {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: pending observed_operation_rversion must be positive", path)
		}
		if state.PendingRuntimeTransition.RuntimeGeneration > state.RuntimeGeneration {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: pending runtime_generation exceeds current runtime_generation", path)
		}
	}
	if state.CleanupPending && state.PendingRuntimeTransition != nil {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: cleanup_pending cannot coexist with pending_runtime_transition", path)
	}
	if len(state.Endpoints) > maxCodespaceEndpoints {
		return codespaceState{}, fmt.Errorf("validate codespace state %s: endpoints exceed limit %d", path, maxCodespaceEndpoints)
	}
	if state.RuntimeMetadata != nil {
		if state.RuntimeMetadata.MetadataGeneration <= 0 {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: metadata_generation must be positive", path)
		}
		if err := validateRuntimeMetadataState(*state.RuntimeMetadata); err != nil {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: %w", path, err)
		}
	}
	seenEndpoints := make(map[string]struct{}, len(state.Endpoints))
	for _, endpoint := range state.Endpoints {
		route, err := normalizeGatewayEndpointRoute(gatewayEndpointRoute{
			codespaceUUID:  codespaceUUID,
			endpointID:     endpoint.EndpointID,
			label:          endpoint.Label,
			upstreamScheme: endpoint.UpstreamScheme,
			upstreamHost:   endpoint.UpstreamHost,
			public:         endpoint.Public,
		})
		if err != nil {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: %w", path, err)
		}
		if _, ok := seenEndpoints[route.endpointID]; ok {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: duplicate endpoint_id %s", path, route.endpointID)
		}
		seenEndpoints[route.endpointID] = struct{}{}
		if err := validateEndpointLabel(route.label); err != nil {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: %w", path, err)
		}
	}
	if state.ActiveOperation != nil {
		if state.ActiveOperation.OperationRVersion <= 0 {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: active operation_rversion must be positive", path)
		}
		switch state.ActiveOperation.WorkerStage {
		case "":
			state.ActiveOperation.WorkerStage = string(manager.OperationWorkerStageActive)
		case string(manager.OperationWorkerStageActive), string(manager.OperationWorkerStageLeasePaused):
		default:
			return codespaceState{}, fmt.Errorf("validate codespace state %s: active operation worker_stage is invalid", path)
		}
		if len(state.ActiveOperation.Payload) == 0 {
			return codespaceState{}, fmt.Errorf("validate codespace state %s: active operation payload is required", path)
		}
	}
	return state, nil
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
	return s.RuntimeGeneration > 0 || s.RuntimeTokenHash != "" || s.PendingRuntimeTransition != nil || s.CleanupPending || len(s.Endpoints) > 0 || s.RuntimeMetadata != nil || s.ActiveOperation != nil
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
		left.UpstreamHost == right.UpstreamHost &&
		left.Public == right.Public
}

func validateRuntimeMetadataSnapshot(snapshot manager.RuntimeMetadataSnapshot) error {
	return validateRuntimeMetadataState(codespaceRuntimeMetadataSnapshot{
		MetadataGeneration: snapshot.MetadataGeneration,
		InternalSSH: codespaceRuntimeMetadataSSH{
			Host:               snapshot.InternalSSH.Host,
			Port:               snapshot.InternalSSH.Port,
			User:               snapshot.InternalSSH.User,
			AuthMode:           snapshot.InternalSSH.AuthMode,
			HostKeyFingerprint: snapshot.InternalSSH.HostKeyFingerprint,
		},
		Boot: codespaceRuntimeMetadataBoot{
			OperationRVersion: snapshot.Boot.OperationRVersion,
			Stage:             snapshot.Boot.Stage,
			StartedUnix:       snapshot.Boot.StartedUnix,
			LastUpdateUnix:    snapshot.Boot.LastUpdateUnix,
		},
	})
}

func validateRuntimeMetadataState(snapshot codespaceRuntimeMetadataSnapshot) error {
	if snapshot.MetadataGeneration <= 0 {
		return fmt.Errorf("metadata_generation must be positive")
	}
	if strings.TrimSpace(snapshot.InternalSSH.Host) == "" {
		return fmt.Errorf("internal ssh host is required")
	}
	if snapshot.InternalSSH.Port < 1 || snapshot.InternalSSH.Port > 65535 {
		return fmt.Errorf("internal ssh port is invalid")
	}
	if strings.TrimSpace(snapshot.InternalSSH.User) == "" {
		return fmt.Errorf("internal ssh user is required")
	}
	if snapshot.InternalSSH.AuthMode != "publickey" {
		return fmt.Errorf("internal ssh auth_mode must be publickey")
	}
	if strings.TrimSpace(snapshot.InternalSSH.HostKeyFingerprint) == "" {
		return fmt.Errorf("internal ssh host key fingerprint is required")
	}
	if snapshot.Boot.OperationRVersion <= 0 {
		return fmt.Errorf("boot operation_rversion must be positive")
	}
	if snapshot.Boot.Stage != "ready" {
		return fmt.Errorf("boot stage must be ready")
	}
	if snapshot.Boot.StartedUnix <= 0 {
		return fmt.Errorf("boot started_unix must be positive")
	}
	if snapshot.Boot.LastUpdateUnix < snapshot.Boot.StartedUnix {
		return fmt.Errorf("boot last_update_unix must be greater than or equal to started_unix")
	}
	return nil
}

func runtimeTokenHash(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("runtime token is required")
	}
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func isRuntimeTokenHash(value string) bool {
	encoded, ok := strings.CutPrefix(value, "sha256:")
	if !ok {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && len(decoded) == sha256.Size
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
