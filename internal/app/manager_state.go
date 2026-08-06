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
	managerRuntimeFileName    = "manager-runtime.json"
	currentProtocolVersion    = 1
)

// ManagerState stores the Manager identity used by the running process.
type ManagerState struct {
	GiteaURL            string
	ManagerID           int64
	ManagerSecret       string
	InventoryGeneration int64
}

type managerRuntimeState struct {
	StateFormatVersion  int   `json:"state_format_version"`
	ProtocolVersion     int32 `json:"protocol_version"`
	InventoryGeneration int64 `json:"inventory_generation"`
}

// Validate checks whether the Manager state is usable.
func (s ManagerState) Validate() error {
	if _, err := normalizeGiteaURL(s.GiteaURL); err != nil {
		return err
	}
	if s.ManagerID <= 0 {
		return fmt.Errorf("manager_id is required")
	}
	if strings.TrimSpace(s.ManagerSecret) == "" {
		return fmt.Errorf("manager_secret is required")
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
	if generation < 0 {
		return fmt.Errorf("inventory_generation must not be negative")
	}
	path, err := managerRuntimeStatePath(s.stateDir)
	if err != nil {
		return err
	}
	return writeJSONFileAtomic(path, managerRuntimeState{
		StateFormatVersion:  managerStateFormatVersion,
		ProtocolVersion:     currentProtocolVersion,
		InventoryGeneration: generation,
	})
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

func loadInventoryGeneration(stateDir string) (int64, error) {
	path, err := managerRuntimeStatePath(stateDir)
	if err != nil {
		return 0, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read manager runtime state %s: %w", path, err)
	}
	var state managerRuntimeState
	if err := json.Unmarshal(content, &state); err != nil {
		return 0, fmt.Errorf("decode manager runtime state %s: %w", path, err)
	}
	if state.StateFormatVersion != managerStateFormatVersion {
		return 0, fmt.Errorf("validate manager runtime state %s: state_format_version must be %d", path, managerStateFormatVersion)
	}
	if state.ProtocolVersion != currentProtocolVersion {
		return 0, fmt.Errorf("validate manager runtime state %s: protocol_version must be %d", path, currentProtocolVersion)
	}
	if state.InventoryGeneration < 0 {
		return 0, fmt.Errorf("validate manager runtime state %s: inventory_generation must not be negative", path)
	}
	return state.InventoryGeneration, nil
}

func managerRuntimeStatePath(stateDir string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return "", fmt.Errorf("manager.state_dir is required")
	}
	return filepath.Join(stateDir, managerRuntimeFileName), nil
}
