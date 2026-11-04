// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"os"
	"path/filepath"
	"testing"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	dockerconfig "github.com/docker/cli/cli/config"
)

func TestDockerCredentialsLifecycle(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "docker")
	const registry = "cache.codespace-system.svc"
	const token = "manager-signed-cache-token"
	cleanup, err := prepareDockerCredentials(directory, map[string]*agentv1.RegistryCredential{
		registry: {Username: "gitea-codespace", Password: token},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("Docker configuration directory permissions = %o", info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Join(directory, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("Docker configuration permissions = %o", info.Mode().Perm())
	}
	loaded, err := dockerconfig.Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	credential := loaded.GetAuthConfigs()[registry]
	if credential.Username != "gitea-codespace" || credential.Password != token || credential.RegistryToken != "" {
		t.Fatalf("loaded Docker credential = %#v", credential)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("Docker credentials remain after cleanup: %v", err)
	}
}

func TestDockerCredentialsRejectInvalidInput(t *testing.T) {
	_, err := prepareDockerCredentials(filepath.Join(t.TempDir(), "docker"), map[string]*agentv1.RegistryCredential{
		"https://cache.example": {Username: "gitea-codespace", Password: "token"},
	})
	if err == nil {
		t.Fatal("invalid registry address was accepted")
	}
}
