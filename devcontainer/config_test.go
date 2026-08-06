// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
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
		"containerEnv": {"HOST_NAME": "${localEnv:HOST_NAME}"},
		"waitFor": "initializeCommand",
	}`)
	configPath := filepath.Join(configDir, "devcontainer.json")
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	resolved, err := Load(LoadOptions{
		Workspace:       workspace,
		AllowedPathRoot: workspace,
		Source: Source{
			Path: ".devcontainer/devcontainer.json",
		},
		LocalEnv: map[string]string{"HOST_NAME": "host-1"},
		ID:       "environment-id",
	})
	if err != nil {
		t.Fatalf("load repository configuration: %v", err)
	}
	if len(resolved.DockerComposeFile) != 2 || resolved.Service != "workspace" {
		t.Fatalf("compose selection = %#v/%q", resolved.DockerComposeFile, resolved.Service)
	}
	if err := resolved.Finalize(); err != nil {
		t.Fatalf("finalize repository configuration: %v", err)
	}
	if resolved.ShutdownAction != "stopCompose" {
		t.Fatalf("Compose shutdown action = %q", resolved.ShutdownAction)
	}
	if resolved.ContainerEnv["HOST_NAME"] != "host-1" || resolved.WorkspaceFolder != "/workspace/"+filepath.Base(workspace) {
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
	if _, err := Load(LoadOptions{
		Workspace:       workspace,
		AllowedPathRoot: workspace,
		Source: Source{
			Path: "devcontainer.json",
		},
		ID: "environment-id",
	}); err == nil {
		t.Fatal("configuration symlink outside workspace was accepted")
	}
}

func TestLoadAllowsConfigurationOutsideWorkspaceWithoutPathPolicy(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "devcontainer.json")
	if err := os.WriteFile(configPath, []byte(`{"image":"debian:12"}`), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	resolved, err := Load(LoadOptions{Workspace: workspace, Source: Source{Path: configPath}, ID: "environment-id"})
	if err != nil {
		t.Fatalf("load external configuration: %v", err)
	}
	if resolved.ConfigurationPath != configPath {
		t.Fatalf("configuration path = %q", resolved.ConfigurationPath)
	}
	if err := resolved.Finalize(); err != nil {
		t.Fatalf("finalize external configuration: %v", err)
	}
	if resolved.ShutdownAction != "stopContainer" {
		t.Fatalf("container shutdown action = %q", resolved.ShutdownAction)
	}
}

func TestLoadImageConfigurationDoesNotSelectCompose(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "devcontainer.json")
	content := []byte(`{
		"image": "mcr.microsoft.com/devcontainers/typescript-node",
		"customizations": {"vscode": {"extensions": ["streetsidesoftware.code-spell-checker"]}},
		"forwardPorts": [3000]
	}`)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	resolved, err := Load(LoadOptions{Workspace: workspace, Source: Source{Path: configPath}, ID: "environment-id"})
	if err != nil {
		t.Fatalf("load image configuration: %v", err)
	}
	if resolved.Image == "" || len(resolved.DockerComposeFile) != 0 {
		t.Fatalf("source selection = image %q, Compose %#v", resolved.Image, resolved.DockerComposeFile)
	}
	if err := resolved.Finalize(); err != nil {
		t.Fatalf("finalize image configuration: %v", err)
	}
	if resolved.WaitFor != LifecycleStageUpdateContent || resolved.UserEnvProbe != "loginInteractiveShell" || resolved.ShutdownAction != "stopContainer" {
		t.Fatalf("image defaults = waitFor %q, userEnvProbe %q, shutdownAction %q", resolved.WaitFor, resolved.UserEnvProbe, resolved.ShutdownAction)
	}
}

func TestLoadBuildConfigurationDoesNotSelectCompose(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "devcontainer.json")
	if err := os.WriteFile(configPath, []byte(`{"build":{"dockerfile":"Dockerfile"}}`), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	resolved, err := Load(LoadOptions{Workspace: workspace, Source: Source{Path: configPath}, ID: "environment-id"})
	if err != nil {
		t.Fatalf("load build configuration: %v", err)
	}
	if resolved.Build == nil || resolved.Build.Dockerfile != "Dockerfile" || resolved.Image != "" || len(resolved.DockerComposeFile) != 0 {
		t.Fatalf("source selection = %#v", resolved.Configuration)
	}
}

func TestLoadAcceptsDockerCommandOptions(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	content := []byte(`{
		"build": {"dockerfile": "Dockerfile", "options": ["--network=host"]},
		"runArgs": ["--hostname", "development"]
	}`)
	if err := os.WriteFile(filepath.Join(workspace, "devcontainer.json"), content, 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	resolved, err := Load(LoadOptions{Workspace: workspace, Source: Source{Path: "devcontainer.json"}})
	if err != nil {
		t.Fatalf("load Docker command options: %v", err)
	}
	if len(resolved.Build.Options) != 1 || len(resolved.RunArgs) != 2 {
		t.Fatalf("Docker command options = %#v / %#v", resolved.Build.Options, resolved.RunArgs)
	}
}
