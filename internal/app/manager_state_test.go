// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerStateRoundTrip(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := SaveManagerState(stateDir, ManagerState{
		GiteaURL:            "https://gitea.example.com/",
		ManagerID:           42,
		ManagerSecret:       "manager-secret",
		RegisteredUnix:      123,
		InventoryGeneration: 7,
	}); err != nil {
		t.Fatalf("save manager state: %v", err)
	}

	state, err := LoadManagerState(stateDir)
	if err != nil {
		t.Fatalf("load manager state: %v", err)
	}
	if state.StateFormatVersion != managerStateFormatVersion || state.ProtocolVersion != currentProtocolVersion ||
		state.GiteaURL != "https://gitea.example.com" || state.ManagerID != 42 ||
		state.ManagerSecret != "manager-secret" || state.RegisteredUnix != 123 || state.InventoryGeneration != 7 {
		t.Fatalf("manager state = %#v", state)
	}
	info, err := os.Stat(filepath.Join(stateDir, managerStateFileName))
	if err != nil {
		t.Fatalf("stat manager state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manager state mode = %v", info.Mode().Perm())
	}
}

func TestManagerStateStorePreservesRegistration(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	saveManagerRegistrationForTest(t, stateDir, "https://gitea.example.com", 42)
	if err := NewManagerStateStore(stateDir).SaveInventoryGeneration(9); err != nil {
		t.Fatalf("save inventory generation: %v", err)
	}
	state, err := LoadManagerState(stateDir)
	if err != nil {
		t.Fatalf("load manager state: %v", err)
	}
	if state.GiteaURL != "https://gitea.example.com" || state.ManagerID != 42 || state.ManagerSecret != "manager-secret" || state.InventoryGeneration != 9 {
		t.Fatalf("manager state = %#v", state)
	}
}

func saveManagerRegistrationForTest(t *testing.T, stateDir, giteaURL string, managerID int64) {
	t.Helper()

	if err := SaveManagerState(stateDir, ManagerState{
		GiteaURL:       giteaURL,
		ManagerID:      managerID,
		ManagerSecret:  "manager-secret",
		RegisteredUnix: 1,
	}); err != nil {
		t.Fatalf("save manager state: %v", err)
	}
}
