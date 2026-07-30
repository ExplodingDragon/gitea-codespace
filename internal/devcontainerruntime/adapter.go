// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

//go:embed builtin/configure-git.sh
var configureGitScript string

//go:embed builtin/start-web-ide.sh
var startWebIDEScript string

const (
	// ContainerRuntimeBinary is the in-container path used by endpoint and TCP bridges.
	ContainerRuntimeBinary     = "/usr/local/libexec/gitea-codespace-runtime"
	codeServerFeatureReference = "ghcr.io/coder/devcontainer-features/code-server:2.0.0"
)

var runtimeMounts = [...]struct {
	path     string
	readOnly bool
}{
	{path: "/var/lib/gitea-codespace/gitea-token", readOnly: true},
	{path: "/var/lib/gitea-codespace/git", readOnly: true},
	{path: "/var/lib/gitea-codespace/bin", readOnly: true},
	{path: "/var/lib/gitea-codespace/runtime"},
}

// BuildCreateOptions translates a Codespace create request into product-neutral engine options.
func BuildCreateOptions(request Request) (containerdocker.CreateOptions, error) {
	mounts := make([]devcontainer.Mount, 0, len(runtimeMounts))
	for _, item := range runtimeMounts {
		if _, err := os.Stat(item.path); err == nil {
			mounts = append(mounts, devcontainer.Mount{Type: "bind", Source: item.path, Target: item.path, ReadOnly: item.readOnly})
		} else if !os.IsNotExist(err) {
			return containerdocker.CreateOptions{}, fmt.Errorf("inspect runtime mount %s: %w", item.path, err)
		}
	}
	injectedFeatures := slices.Clone(request.InjectedFeatures)
	injectedFeatures = append(injectedFeatures, devcontainer.InjectedFeature{
		Reference: codeServerFeatureReference,
		Origin:    "platform",
		Options: map[string]json.RawMessage{
			"version":            json.RawMessage(strconv.Quote(request.CodeServerVersion)),
			"auth":               json.RawMessage(`"none"`),
			"host":               json.RawMessage(`"0.0.0.0"`),
			"port":               json.RawMessage(strconv.Quote(fmt.Sprint(runtimeendpoint.WorkspaceEndpointPort))),
			"disableTelemetry":   json.RawMessage("true"),
			"disableUpdateCheck": json.RawMessage("true"),
		},
	})
	return containerdocker.CreateOptions{
		OwnerID:          request.CodespaceUUID,
		Workspace:        request.Workspace,
		Source:           request.Source,
		AllowedPathRoot:  request.Workspace,
		HostUser:         request.HostUser,
		LocalEnvironment: request.LocalEnvironment,
		Secrets:          request.Secrets,
		InjectedFeatures: injectedFeatures,
		AdditionalMounts: mounts,
		Labels:           map[string]string{"dev.gitea.codespace.uuid": request.CodespaceUUID},
	}, nil
}

// ConfigureCreate installs the runtime bridge and the immutable Git identity before lifecycle commands run.
func ConfigureCreate(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, request Request) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate runtime binary: %w", err)
	}
	if err := engine.CopyFile(ctx, state.PrimaryContainerID, executable, ContainerRuntimeBinary, 0o755); err != nil {
		return err
	}
	values := map[string]string{
		"GITEA_GIT_USER_NAME":  request.GitUserName,
		"GITEA_GIT_USER_EMAIL": request.GitUserEmail,
	}
	if _, _, err := engine.Exec(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/bash", "-c", configureGitScript}, values, nil); err != nil {
		return fmt.Errorf("configure Git in Dev Container: %w", err)
	}
	return nil
}

// StartWorkspaceServices runs attach lifecycle commands and makes the Web IDE and declared endpoints available.
func StartWorkspaceServices(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, secrets map[string]string) error {
	if err := engine.RunPostAttach(ctx, state, secrets); err != nil {
		return err
	}
	customizations := struct {
		Settings   map[string]any `json:"settings"`
		Extensions []string       `json:"extensions"`
	}{}
	if raw := state.Configuration.Customizations["vscode"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &customizations); err != nil {
			return fmt.Errorf("decode VS Code customizations: %w", err)
		}
	}
	settings := []byte("{}")
	if customizations.Settings != nil {
		var err error
		settings, err = json.Marshal(customizations.Settings)
		if err != nil {
			return err
		}
	}
	values := map[string]string{
		"GITEA_WEB_IDE_PORT": fmt.Sprint(runtimeendpoint.WorkspaceEndpointPort),
		"GITEA_WORKSPACE":    state.RemoteWorkdir,
	}
	if _, _, err := engine.Exec(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/bash", "-c", startWebIDEScript}, values, bytes.NewReader(settings)); err != nil {
		return fmt.Errorf("start platform Web IDE: %w", err)
	}
	for _, extension := range customizations.Extensions {
		extension = strings.TrimSpace(extension)
		if extension == "" {
			return fmt.Errorf("VS Code extension identifier is empty")
		}
		if _, _, err := engine.Exec(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"code-server", "--install-extension", extension}, nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: install VS Code extension %s: %v\n", extension, err)
		}
	}
	return WriteConfiguredEndpoints(state.Configuration)
}
