// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerCredentialsRoundTrip(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := SaveManagerCredentials(stateDir, ManagerCredentials{
		ManagerID:     42,
		ManagerSecret: "manager-secret",
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	credentials, err := LoadManagerCredentials(stateDir)
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}
	if credentials.FormatVersion != managerCredentialsFormatVersion {
		t.Fatalf("format version = %d", credentials.FormatVersion)
	}
	if credentials.ManagerID != 42 {
		t.Fatalf("manager id = %d", credentials.ManagerID)
	}
	if credentials.ManagerSecret != "manager-secret" {
		t.Fatalf("manager secret = %q", credentials.ManagerSecret)
	}

	info, err := os.Stat(filepath.Join(stateDir, managerCredentialsFileName))
	if err != nil {
		t.Fatalf("stat credentials: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %v", info.Mode().Perm())
	}
}

func TestManagerCredentialsRejectWrongFormat(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	path := filepath.Join(stateDir, managerCredentialsFileName)
	if err := os.WriteFile(path, []byte(`{"format_version":2,"manager_id":1,"manager_secret":"secret"}`), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	if _, err := LoadManagerCredentials(stateDir); err == nil {
		t.Fatalf("expected wrong format error")
	}
}
