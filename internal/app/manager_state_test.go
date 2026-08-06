// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerStateStorePersistsInventoryOnly(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := NewManagerStateStore(stateDir).SaveInventoryGeneration(7); err != nil {
		t.Fatalf("save inventory generation: %v", err)
	}

	generation, err := loadInventoryGeneration(stateDir)
	if err != nil {
		t.Fatalf("load inventory generation: %v", err)
	}
	if generation != 7 {
		t.Fatalf("inventory generation = %d", generation)
	}
	info, err := os.Stat(filepath.Join(stateDir, managerRuntimeFileName))
	if err != nil {
		t.Fatalf("stat manager runtime state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manager runtime state mode = %v", info.Mode().Perm())
	}
}

func TestManagerStateValidatesIdentity(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := NewManagerStateStore(stateDir).SaveInventoryGeneration(9); err != nil {
		t.Fatalf("save inventory generation: %v", err)
	}
	generation, err := loadInventoryGeneration(stateDir)
	if err != nil {
		t.Fatalf("load inventory generation: %v", err)
	}
	state := testManagerState(t, stateDir, "https://gitea.example.com/", 42)
	state.InventoryGeneration = generation
	if err := state.Validate(); err != nil {
		t.Fatalf("validate manager state: %v", err)
	}
	if state.GiteaURL != "https://gitea.example.com" || state.ManagerID != 42 || state.ManagerSecret != "manager-secret" || state.InventoryGeneration != 9 {
		t.Fatalf("manager state = %#v", state)
	}
}

func saveManagerStateForTest(t *testing.T, stateDir, giteaURL string, managerID int64) ManagerState {
	t.Helper()
	if err := NewManagerStateStore(stateDir).SaveInventoryGeneration(0); err != nil {
		t.Fatalf("save inventory generation: %v", err)
	}
	return testManagerState(t, stateDir, giteaURL, managerID)
}

func testManagerState(t *testing.T, stateDir, giteaURL string, managerID int64) ManagerState {
	t.Helper()
	normalized, err := normalizeGiteaURL(giteaURL)
	if err != nil {
		t.Fatalf("normalize test Gitea URL: %v", err)
	}
	generation, err := loadInventoryGeneration(stateDir)
	if err != nil {
		t.Fatalf("load inventory generation: %v", err)
	}
	return ManagerState{
		GiteaURL:            normalized,
		ManagerID:           managerID,
		ManagerSecret:       "manager-secret",
		InventoryGeneration: generation,
	}
}
