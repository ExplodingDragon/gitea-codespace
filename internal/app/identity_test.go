// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerIdentityRoundTrip(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := SaveManagerIdentity(stateDir, ManagerIdentity{
		GiteaURL:       "https://gitea.example.com/",
		ManagerID:      42,
		RegisteredUnix: 123,
	}); err != nil {
		t.Fatalf("save identity: %v", err)
	}

	identity, err := LoadManagerIdentity(stateDir)
	if err != nil {
		t.Fatalf("load identity: %v", err)
	}
	if identity.StateFormatVersion != managerIdentityFormatVersion {
		t.Fatalf("state format version = %d", identity.StateFormatVersion)
	}
	if identity.ProtocolVersion != currentProtocolVersion {
		t.Fatalf("protocol version = %d", identity.ProtocolVersion)
	}
	if identity.GiteaURL != "https://gitea.example.com" {
		t.Fatalf("gitea url = %q", identity.GiteaURL)
	}
	if identity.ManagerID != 42 {
		t.Fatalf("manager id = %d", identity.ManagerID)
	}
	if identity.RegisteredUnix != 123 {
		t.Fatalf("registered unix = %d", identity.RegisteredUnix)
	}

	info, err := os.Stat(filepath.Join(stateDir, managerIdentityFileName))
	if err != nil {
		t.Fatalf("stat identity: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %v", info.Mode().Perm())
	}
}

func TestManagerIdentityRejectsWrongProtocolVersion(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	path := filepath.Join(stateDir, managerIdentityFileName)
	content := `{"state_format_version":1,"protocol_version":2,"gitea_url":"https://gitea.example.com","manager_id":42,"registered_unix":123}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	if _, err := LoadManagerIdentity(stateDir); err == nil {
		t.Fatalf("expected protocol version error")
	}
}

func TestManagerIdentityRejectsOverwrite(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	identity := ManagerIdentity{
		GiteaURL:       "https://gitea.example.com",
		ManagerID:      42,
		RegisteredUnix: 123,
	}
	if err := SaveManagerIdentity(stateDir, identity); err != nil {
		t.Fatalf("save identity: %v", err)
	}
	if err := SaveManagerIdentity(stateDir, identity); err == nil {
		t.Fatalf("expected overwrite error")
	}
}
