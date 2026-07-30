// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	managerStateFormatVersion = 1
	managerStateFileName      = "manager-state.json"
	currentProtocolVersion    = 1
)

// ManagerState stores the registered Manager identity and inventory generation.
type ManagerState struct {
	StateFormatVersion  int    `json:"state_format_version"`
	ProtocolVersion     int32  `json:"protocol_version"`
	GiteaURL            string `json:"gitea_url"`
	ManagerID           int64  `json:"manager_id"`
	ManagerSecret       string `json:"manager_secret"`
	RegisteredUnix      int64  `json:"registered_unix"`
	InventoryGeneration int64  `json:"inventory_generation"`
}

// LoadManagerState loads the registered Manager state from stateDir.
func LoadManagerState(stateDir string) (ManagerState, error) {
	path, err := managerStatePath(stateDir)
	if err != nil {
		return ManagerState{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return ManagerState{}, fmt.Errorf("read manager state %s: %w", path, err)
	}
	var state ManagerState
	if err := json.Unmarshal(content, &state); err != nil {
		return ManagerState{}, fmt.Errorf("decode manager state %s: %w", path, err)
	}
	if err := state.Validate(); err != nil {
		return ManagerState{}, fmt.Errorf("validate manager state %s: %w", path, err)
	}
	return state, nil
}

// SaveManagerState atomically stores the registered Manager state in stateDir.
func SaveManagerState(stateDir string, state ManagerState) error {
	path, err := managerStatePath(stateDir)
	if err != nil {
		return err
	}
	state.StateFormatVersion = managerStateFormatVersion
	state.ProtocolVersion = currentProtocolVersion
	state.GiteaURL, err = normalizeGiteaURL(state.GiteaURL)
	if err != nil {
		return err
	}
	if err := state.Validate(); err != nil {
		return err
	}
	return writeJSONFileAtomic(path, state)
}

// Validate checks whether the Manager state is usable.
func (s ManagerState) Validate() error {
	if s.StateFormatVersion != managerStateFormatVersion {
		return fmt.Errorf("state_format_version must be %d", managerStateFormatVersion)
	}
	if s.ProtocolVersion != currentProtocolVersion {
		return fmt.Errorf("protocol_version must be %d", currentProtocolVersion)
	}
	if _, err := normalizeGiteaURL(s.GiteaURL); err != nil {
		return err
	}
	if s.ManagerID <= 0 {
		return fmt.Errorf("manager_id is required")
	}
	if strings.TrimSpace(s.ManagerSecret) == "" {
		return fmt.Errorf("manager_secret is required")
	}
	if s.RegisteredUnix <= 0 {
		return fmt.Errorf("registered_unix is required")
	}
	if s.InventoryGeneration < 0 {
		return fmt.Errorf("inventory_generation must not be negative")
	}
	return nil
}

// ManagerStateStore persists Manager-wide inventory state updates.
type ManagerStateStore struct {
	stateDir string
}

// NewManagerStateStore creates a Manager state store for one state directory.
func NewManagerStateStore(stateDir string) *ManagerStateStore {
	return &ManagerStateStore{stateDir: stateDir}
}

// SaveInventoryGeneration persists the latest allocated inventory generation.
func (s *ManagerStateStore) SaveInventoryGeneration(generation int64) error {
	state, err := LoadManagerState(s.stateDir)
	if err != nil {
		return err
	}
	state.InventoryGeneration = generation
	return SaveManagerState(s.stateDir, state)
}

func normalizeGiteaURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("gitea_url must be an absolute URL")
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("gitea_url scheme must be http or https")
	}
	return value, nil
}

func managerStatePath(stateDir string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return "", fmt.Errorf("manager.state_dir is required")
	}
	return filepath.Join(stateDir, managerStateFileName), nil
}
