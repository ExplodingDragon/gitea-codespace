// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package devcontainerruntime translates Manager Runtime configuration into
// Dev Container engine operations and Agent endpoint state.
package devcontainerruntime

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"gitea.dev/codespace/internal/webide"
)

//go:embed builtin/configure-git.sh
var configureGitScript string

//go:embed builtin/install-ca.sh
var installCAScript string

//go:embed builtin/endpoint.sh
var endpointScript []byte

const (
	// ContainerRuntimeBinary is the in-container path used by endpoint and TCP bridges.
	ContainerRuntimeBinary  = "/usr/local/libexec/gitea-codespace-runtime"
	containerEndpointBinary = "/usr/local/bin/gitea-codespace-endpoint"
	containerCABundlePath   = "/var/lib/gitea-codespace/ca/bundle.pem"
)

var runtimeMounts = [...]struct {
	source   string
	target   string
	readOnly bool
}{
	{source: "/run/codespace/gitea-token", target: "/var/lib/gitea-codespace/gitea-token", readOnly: true},
	{source: "/run/codespace/git", target: "/var/lib/gitea-codespace/git", readOnly: true},
	{source: "/run/codespace/ca", target: "/var/lib/gitea-codespace/ca", readOnly: true},
	{source: runtimeendpoint.AgentSocketPath, target: runtimeendpoint.ContainerSocketPath},
	{source: "/usr/local/bin/gitea-codespace", target: ContainerRuntimeBinary, readOnly: true},
}

func buildCreateOptions(request Request) (containerdocker.CreateOptions, error) {
	mounts := make([]devcontainer.Mount, 0, len(runtimeMounts)+4)
	for _, item := range runtimeMounts {
		if _, err := os.Stat(item.source); err == nil {
			mounts = append(mounts, devcontainer.Mount{Type: "bind", Source: item.source, Target: item.target, ReadOnly: item.readOnly})
		} else if !os.IsNotExist(err) {
			return containerdocker.CreateOptions{}, fmt.Errorf("inspect runtime mount %s: %w", item.source, err)
		}
	}
	if request.DevContainer.WebIDE.Enabled() {
		mounts = append(mounts,
			devcontainer.Mount{Type: "bind", Source: webide.ProgramRootPath, Target: webide.ContainerProgramPath, ReadOnly: true},
			devcontainer.Mount{Type: "bind", Source: webide.ProviderDataPath(string(request.DevContainer.WebIDE.Provider)), Target: webide.ContainerDataPath},
		)
	}
	for _, item := range request.Native.Mounts {
		mounts = append(mounts, devcontainer.Mount{Type: "bind", Source: item.Source, Target: item.Target, ReadOnly: item.ReadOnly})
	}
	injected := make([]devcontainer.InjectedFeature, 0, len(request.DevContainer.Features))
	for _, feature := range request.DevContainer.Features {
		options := map[string]json.RawMessage{}
		if len(feature.Options.Raw) != 0 {
			if err := json.Unmarshal(feature.Options.Raw, &options); err != nil {
				return containerdocker.CreateOptions{}, fmt.Errorf("decode Dev Container Feature %s options: %w", feature.Reference, err)
			}
		}
		injected = append(injected, devcontainer.InjectedFeature{Reference: strings.TrimSpace(feature.Reference), Origin: "environment template", Options: options})
	}
	containerEnvironment := map[string]string{
		"GITEA_INSTANCE_URL":  request.GiteaURL,
		"NODE_EXTRA_CA_CERTS": containerCABundlePath,
		"SSL_CERT_FILE":       containerCABundlePath,
	}
	if request.DevContainer.GiteaCLI {
		containerEnvironment["GITEA_CLI_NO_UPDATE_NOTIFIER"] = "1"
	}
	platformEnvironment := map[string]string{}
	for _, name := range devcontainer.ProxyVariableNames() {
		if value := request.LocalEnvironment[name]; value != "" {
			platformEnvironment[name] = value
		}
	}
	deviceMappings := make([]containerdocker.DeviceMapping, 0, len(request.Native.Devices))
	for _, mapping := range request.Native.Devices {
		deviceMappings = append(deviceMappings, containerdocker.DeviceMapping{Source: mapping.Source, Target: mapping.Target, Permissions: mapping.Permissions})
	}
	return containerdocker.CreateOptions{
		OwnerID:             request.CodespaceUUID,
		Workspace:           request.Workspace,
		Source:              request.Source,
		AllowedPathRoot:     request.Workspace,
		HostUser:            request.HostUser,
		LocalEnvironment:    request.LocalEnvironment,
		PlatformEnvironment: platformEnvironment,
		Secrets:             request.Secrets,
		Cache:               request.Cache,
		RunLifecycle:        request.RunLifecycle,
		InjectedFeatures:    injected,
		AdditionalMounts:    mounts,
		ContainerEnv:        containerEnvironment,
		Labels:              map[string]string{"dev.gitea.codespace.uuid": request.CodespaceUUID},
		PlatformDevices: containerdocker.PlatformDevices{
			CDI: request.Native.CDIDeviceIDs, Mappings: deviceMappings, Capabilities: request.Native.Capabilities, ProvidesGPU: request.Native.ProvidesGPU,
		},
	}, nil
}

func configureCreate(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, request Request) error {
	if err := configureCA(ctx, engine, state); err != nil {
		return err
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, "0", "/", []string{"mkdir", "-p", "/usr/local/libexec", "/usr/local/bin"}, nil, nil); err != nil {
		return fmt.Errorf("prepare platform directories in Dev Container: %w", err)
	}
	if err := engine.CopyContent(ctx, state.PrimaryContainerID, containerEndpointBinary, 0o755, endpointScript); err != nil {
		return err
	}
	values := map[string]string{
		"GITEA_URL":            request.GiteaURL,
		"GITEA_GIT_USER_NAME":  request.GitUserName,
		"GITEA_GIT_USER_EMAIL": request.GitUserEmail,
		"GITEA_WORKSPACE":      state.RemoteWorkdir,
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/sh", "-c", configureGitScript}, values, nil); err != nil {
		return fmt.Errorf("configure Git in Dev Container: %w", err)
	}
	return nil
}

func configureCA(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
	if err := engine.Run(ctx, state.PrimaryContainerID, "0", "/", []string{"/bin/sh", "-c", installCAScript}, nil, nil); err != nil {
		return fmt.Errorf("configure platform CA in Dev Container: %w", err)
	}
	if state.RemoteEnvironment == nil {
		state.RemoteEnvironment = map[string]string{}
	}
	state.RemoteEnvironment["NODE_EXTRA_CA_CERTS"] = containerCABundlePath
	state.RemoteEnvironment["SSL_CERT_FILE"] = containerCABundlePath
	return nil
}

func startWebIDE(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, secrets map[string]string, configuration Configuration, initialize bool, stderr io.Writer) bool {
	if !configuration.WebIDE.Enabled() {
		return false
	}
	values := devcontainer.ProcessEnvironment(state.RemoteEnvironment, secrets)
	values["CODESPACE_WEB_IDE_PROVIDER"] = string(configuration.WebIDE.Provider)
	values["CODESPACE_WEB_IDE_PROGRAM_DIR"] = webide.ContainerProgramPath
	values["CODESPACE_WEB_IDE_DATA_DIR"] = webide.ContainerDataPath
	values["CODESPACE_WEB_IDE_WORKSPACE"] = state.RemoteWorkdir
	values["CODESPACE_WEB_IDE_ADDRESS"] = fmt.Sprintf("127.0.0.1:%d", runtimeendpoint.WorkspaceEndpointPort)
	templateOptions := json.RawMessage(configuration.WebIDE.Options.Raw)
	if len(templateOptions) == 0 {
		templateOptions = json.RawMessage(`{}`)
	}
	repositoryOptions := state.Configuration.Customizations[string(configuration.WebIDE.Provider)]
	if len(repositoryOptions) == 0 {
		repositoryOptions = json.RawMessage(`{}`)
	}
	payload, startErr := json.Marshal(struct {
		ProtocolVersion   int             `json:"protocolVersion"`
		Initialize        bool            `json:"initialize"`
		TemplateOptions   json.RawMessage `json:"templateOptions"`
		RepositoryOptions json.RawMessage `json:"repositoryOptions"`
	}{1, initialize, templateOptions, repositoryOptions})
	if startErr == nil {
		prepare := webide.ContainerProgramPath + "/" + webide.PrepareProgram
		if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{prepare}, values, bytes.NewReader(payload)); err != nil {
			startErr = fmt.Errorf("prepare %s Web IDE: %w", configuration.WebIDE.Provider, err)
		}
	}
	if startErr == nil {
		serve := webide.ContainerProgramPath + "/" + webide.ServeProgram
		if err := engine.RunDetached(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{serve}, values); err != nil {
			startErr = fmt.Errorf("start %s Web IDE Provider: %w", configuration.WebIDE.Provider, err)
		}
	}
	if startErr != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: Web IDE did not start and Codespace access remains available: %v\n", startErr)
		return false
	}
	if err := engine.RunPostAttach(ctx, state, secrets); err != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: Web IDE postAttachCommand failed without affecting Codespace access: %v\n", err)
	}
	return true
}
