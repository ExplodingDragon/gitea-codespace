// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	managerRootStateFormatVersion = 1
	managerRootStateFileName      = "root-state.json"
)

// ManagerRootState stores the root Manager state directory snapshot.
type ManagerRootState struct {
	StateFormatVersion  int   `json:"state_format_version"`
	ManagerID           int64 `json:"manager_id"`
	InventoryGeneration int64 `json:"inventory_generation"`
}

// ManagerRootStateStore persists Manager-wide root state updates.
type ManagerRootStateStore struct {
	stateDir  string
	managerID int64
}

// NewManagerRootStateStore creates a root state store for one Manager identity.
func NewManagerRootStateStore(stateDir string, managerID int64) *ManagerRootStateStore {
	return &ManagerRootStateStore{
		stateDir:  stateDir,
		managerID: managerID,
	}
}

// SaveInventoryGeneration persists the latest allocated inventory generation.
func (s *ManagerRootStateStore) SaveInventoryGeneration(generation int64) error {
	return SaveManagerRootState(s.stateDir, ManagerRootState{
		ManagerID:           s.managerID,
		InventoryGeneration: generation,
	})
}

// LoadManagerRootState loads and validates root-state.json from stateDir.
func LoadManagerRootState(stateDir string, identity ManagerIdentity) (ManagerRootState, error) {
	path, err := managerRootStatePath(stateDir)
	if err != nil {
		return ManagerRootState{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return ManagerRootState{}, fmt.Errorf("read manager root state %s: %w", path, err)
	}
	var state ManagerRootState
	if err := json.Unmarshal(content, &state); err != nil {
		return ManagerRootState{}, fmt.Errorf("decode manager root state %s: %w", path, err)
	}
	if err := state.Validate(identity); err != nil {
		return ManagerRootState{}, fmt.Errorf("validate manager root state %s: %w", path, err)
	}
	return state, nil
}

// SaveManagerRootState stores root-state.json in stateDir.
func SaveManagerRootState(stateDir string, state ManagerRootState) error {
	path, err := managerRootStatePath(stateDir)
	if err != nil {
		return err
	}
	state.StateFormatVersion = managerRootStateFormatVersion
	if err := state.validateFields(); err != nil {
		return err
	}
	return writeJSONFileAtomic(path, state)
}

// Validate checks whether root-state.json matches the local identity.
func (s ManagerRootState) Validate(identity ManagerIdentity) error {
	if err := s.validateFields(); err != nil {
		return err
	}
	if identity.ManagerID <= 0 {
		return fmt.Errorf("identity manager_id is required")
	}
	if s.ManagerID != identity.ManagerID {
		return fmt.Errorf("manager_id %d does not match identity manager_id %d", s.ManagerID, identity.ManagerID)
	}
	return nil
}

func (s ManagerRootState) validateFields() error {
	if s.StateFormatVersion != managerRootStateFormatVersion {
		return fmt.Errorf("state_format_version must be %d", managerRootStateFormatVersion)
	}
	if s.ManagerID <= 0 {
		return fmt.Errorf("manager_id is required")
	}
	if s.InventoryGeneration < 0 {
		return fmt.Errorf("inventory_generation must not be negative")
	}
	return nil
}

func managerRootStatePath(stateDir string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return "", fmt.Errorf("manager.state_dir is required")
	}
	return filepath.Join(stateDir, managerRootStateFileName), nil
}
