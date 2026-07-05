// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.dev/codespace/internal/controlplane"
	"gitea.dev/codespace/internal/store"
)

func TestRegisterWritesManagerCredentials(t *testing.T) {
	t.Parallel()

	memoryStore := store.New()
	if err := memoryStore.EnsureRegistrationToken("registration-token", "test", time.Hour); err != nil {
		t.Fatalf("ensure registration token: %v", err)
	}
	server := newTestServer(t, memoryStore)

	workdir := t.TempDir()
	input := bytes.NewBufferString(server.URL + "\nregistration-token\nregistered-manager\n")
	var output bytes.Buffer
	if err := Register(&output, input, filepath.Join(workdir, "existing.json")); err != nil {
		t.Fatalf("register: %v", err)
	}

	configPath := filepath.Join(workdir, defaultRegisterConfigPath)
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("stat generated config: %v", err)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	if config.Gitea.URL != server.URL {
		t.Fatalf("gitea url = %q", config.Gitea.URL)
	}
	if config.Manager.Name != "registered-manager" {
		t.Fatalf("manager name = %q", config.Manager.Name)
	}
	if config.Manager.UUID == "" {
		t.Fatalf("manager uuid is empty")
	}
	if config.Manager.Token == "" || config.Manager.Token == "registration-token" {
		t.Fatalf("manager token was not replaced")
	}
}

func newTestServer(t *testing.T, memoryStore *store.MemoryStore) *httptest.Server {
	t.Helper()
	return httptest.NewServer(newHTTPHandler(DefaultConfig(), memoryStore, controlplane.New(memoryStore)))
}
