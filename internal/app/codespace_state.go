// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
)

const (
	codespaceStateFormatVersion = 1
	codespaceStateDirName       = "codespaces"
)

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
	PendingRuntimeTransition *codespacePendingRuntimeTransition `json:"pending_runtime_transition,omitempty"`
	CleanupPending           bool                               `json:"cleanup_pending,omitempty"`
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
	return s.RuntimeGeneration > 0 || s.PendingRuntimeTransition != nil || s.CleanupPending || s.ActiveOperation != nil
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
