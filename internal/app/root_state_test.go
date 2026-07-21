// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerRootStateRoundTrip(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := SaveManagerRootState(stateDir, ManagerRootState{
		ManagerID:           42,
		InventoryGeneration: 7,
	}); err != nil {
		t.Fatalf("save root state: %v", err)
	}

	state, err := LoadManagerRootState(stateDir, ManagerCredentials{
		FormatVersion: managerCredentialsFormatVersion,
		ManagerID:     42,
		ManagerSecret: "manager-secret",
	})
	if err != nil {
		t.Fatalf("load root state: %v", err)
	}
	if state.StateFormatVersion != managerRootStateFormatVersion {
		t.Fatalf("state format version = %d", state.StateFormatVersion)
	}
	if state.InventoryGeneration != 7 {
		t.Fatalf("inventory generation = %d", state.InventoryGeneration)
	}

	info, err := os.Stat(filepath.Join(stateDir, managerRootStateFileName))
	if err != nil {
		t.Fatalf("stat root state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("root state mode = %v", info.Mode().Perm())
	}
}

func TestManagerRootStateRejectsMismatch(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := SaveManagerRootState(stateDir, ManagerRootState{
		ManagerID: 42,
	}); err != nil {
		t.Fatalf("save root state: %v", err)
	}
	_, err := LoadManagerRootState(stateDir, ManagerCredentials{
		FormatVersion: managerCredentialsFormatVersion,
		ManagerID:     43,
		ManagerSecret: "manager-secret",
	})
	if err == nil {
		t.Fatalf("expected manager id mismatch error")
	}
}

func TestManagerRootStateRejectsWrongFormat(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	path := filepath.Join(stateDir, managerRootStateFileName)
	if err := os.WriteFile(path, []byte(`{"state_format_version":2,"manager_id":42,"inventory_generation":0}`), 0o600); err != nil {
		t.Fatalf("write root state: %v", err)
	}
	_, err := LoadManagerRootState(stateDir, ManagerCredentials{
		FormatVersion: managerCredentialsFormatVersion,
		ManagerID:     42,
		ManagerSecret: "manager-secret",
	})
	if err == nil {
		t.Fatalf("expected wrong format error")
	}
}
