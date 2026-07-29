// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRepositoryConfiguration(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	configDir := filepath.Join(workspace, ".devcontainer")
	if err := os.Mkdir(configDir, 0o755); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	content := []byte(`{
		// JSONC comments and a trailing comma are accepted.
		"dockerComposeFile": ["compose.yaml", "compose.override.yaml"],
		"service": "workspace",
		"workspaceFolder": "/workspace/${localWorkspaceFolderBasename}",
		"containerEnv": {"MANAGER": "${localEnv:MANAGER_NAME}"},
		"waitFor": "initializeCommand",
	}`)
	configPath := filepath.Join(configDir, "devcontainer.json")
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	digest := sha256.Sum256(content)
	resolved, err := Load(workspace, Selection{
		Source:        "repository",
		Path:          ".devcontainer/devcontainer.json",
		CommitSHA:     "0123456789abcdef0123456789abcdef01234567",
		ContentSHA256: hex.EncodeToString(digest[:]),
	}, map[string]string{"MANAGER_NAME": "manager-1"}, "environment-id")
	if err != nil {
		t.Fatalf("load repository configuration: %v", err)
	}
	if len(resolved.DockerComposeFile) != 2 || resolved.Service != "workspace" {
		t.Fatalf("compose selection = %#v/%q", resolved.DockerComposeFile, resolved.Service)
	}
	if resolved.ContainerEnv["MANAGER"] != "manager-1" || resolved.WorkspaceFolder != "/workspace/"+filepath.Base(workspace) {
		t.Fatalf("resolved variables = %#v, workspace=%q", resolved.ContainerEnv, resolved.WorkspaceFolder)
	}
}

func TestLoadRejectsConfigurationSymlinkOutsideWorkspace(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "devcontainer.json")
	content := []byte(`{"image":"debian:12"}`)
	if err := os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatalf("write outside config: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "devcontainer.json")); err != nil {
		t.Fatalf("create config symlink: %v", err)
	}
	digest := sha256.Sum256(content)
	if _, err := Load(workspace, Selection{
		Source:        "repository",
		Path:          "devcontainer.json",
		ContentSHA256: hex.EncodeToString(digest[:]),
	}, nil, "environment-id"); err == nil {
		t.Fatal("configuration symlink outside workspace was accepted")
	}
}
