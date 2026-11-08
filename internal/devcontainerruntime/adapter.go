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
	"errors"
	"fmt"
	"io"
	"maps"
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
	if request.DevContainer.WebIDE.Enabled {
		programSource := webide.BuiltinProgramPath
		if strings.TrimSpace(request.DevContainer.WebIDE.ArchiveURL) != "" {
			programSource = webide.CustomProgramPath
		}
		for _, item := range []struct {
			source, target string
			readOnly       bool
		}{
			{programSource, webide.ContainerProgramPath, true},
			{webide.UserDataSourcePath, webide.ContainerUserDataPath, false},
			{webide.ExtensionsSourcePath, webide.ContainerExtensionsPath, false},
			{webide.PlatformVSIXDirectory, webide.ContainerVSIXDirectory, true},
		} {
			mounts = append(mounts, devcontainer.Mount{Type: "bind", Source: item.source, Target: item.target, ReadOnly: item.readOnly})
		}
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
		"CODESPACE_CA_BUNDLE": containerCABundlePath,
		"GITEA_INSTANCE_URL":  request.GiteaURL,
		"NODE_EXTRA_CA_CERTS": containerCABundlePath,
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
	if request.GiteaCustomCA {
		containerEnvironment["GITEA_CA_BUNDLE"] = containerCABundlePath
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
	if err := configureCA(ctx, engine, state, request.GiteaCustomCA); err != nil {
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
	if request.GiteaCustomCA {
		values["GITEA_CA_BUNDLE"] = containerCABundlePath
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/sh", "-c", configureGitScript}, values, nil); err != nil {
		return fmt.Errorf("configure Git in Dev Container: %w", err)
	}
	return nil
}

func configureCA(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, giteaCustom bool) error {
	if err := engine.Run(ctx, state.PrimaryContainerID, "0", "/", []string{"/bin/sh", "-c", installCAScript}, nil, nil); err != nil {
		return fmt.Errorf("configure platform CA in Dev Container: %w", err)
	}
	if state.RemoteEnvironment == nil {
		state.RemoteEnvironment = map[string]string{}
	}
	state.RemoteEnvironment["CODESPACE_CA_BUNDLE"] = containerCABundlePath
	state.RemoteEnvironment["NODE_EXTRA_CA_CERTS"] = containerCABundlePath
	if giteaCustom {
		state.RemoteEnvironment["GITEA_CA_BUNDLE"] = containerCABundlePath
	} else {
		delete(state.RemoteEnvironment, "GITEA_CA_BUNDLE")
	}
	return nil
}

func startWebIDE(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, secrets map[string]string, configuration Configuration, initialize bool, stdout, stderr io.Writer) {
	if !configuration.WebIDE.Enabled {
		return
	}
	values := devcontainer.ProcessEnvironment(state.RemoteEnvironment, secrets)
	var startErr error
	if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{ContainerRuntimeBinary, "runtime", "web-ide", "ensure-extension"}, values, nil); err != nil {
		startErr = fmt.Errorf("install required Codespace extension: %w", err)
	}
	if initialize {
		customizations, err := resolveWebIDECustomizations(configuration.WebIDE, state.Configuration.Customizations["vscode"])
		if err != nil {
			startErr = errors.Join(startErr, err)
		}
		if startErr == nil {
			settings, err := json.Marshal(customizations.Settings)
			if err != nil {
				startErr = fmt.Errorf("encode Web IDE settings: %w", err)
			} else if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{ContainerRuntimeBinary, "runtime", "web-ide", "prepare"}, values, bytes.NewReader(settings)); err != nil {
				startErr = fmt.Errorf("initialize platform Web IDE settings: %w", err)
			}
		}
		if startErr == nil {
			if len(customizations.Extensions) > 0 {
				_, _ = fmt.Fprintln(stdout, "Install VS Code extensions")
			}
			codeServer := webide.ContainerProgramPath + "/bin/code-server"
			for _, extension := range customizations.Extensions {
				if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{codeServer,
					"--user-data-dir", webide.ContainerUserDataPath,
					"--extensions-dir", webide.ContainerExtensionsPath,
					"--force", "--install-extension", extension,
				}, values, nil); err != nil {
					_, _ = fmt.Fprintf(stderr, "Warning: Install VS Code extension %s: %v\n", extension, err)
				}
			}
		}
	}
	if startErr == nil {
		if err := engine.RunDetached(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{ContainerRuntimeBinary, "runtime", "web-ide", "serve"}, values); err != nil {
			startErr = fmt.Errorf("start platform Web IDE: %w", err)
		}
	}
	if startErr != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: Web IDE is unavailable and will not affect Codespace access: %v\n", startErr)
	}
	if err := engine.RunPostAttach(ctx, state, secrets); err != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: Web IDE postAttachCommand failed without affecting Codespace access: %v\n", err)
	}
}

type webIDECustomizations struct {
	Settings   map[string]json.RawMessage `json:"settings"`
	Extensions []string                   `json:"extensions"`
}

func resolveWebIDECustomizations(configuration WebIDEConfiguration, repository json.RawMessage) (webIDECustomizations, error) {
	result := webIDECustomizations{Settings: map[string]json.RawMessage{}, Extensions: append([]string(nil), configuration.Extensions...)}
	if len(configuration.Settings.Raw) != 0 {
		if err := json.Unmarshal(configuration.Settings.Raw, &result.Settings); err != nil {
			return webIDECustomizations{}, fmt.Errorf("decode environment-template Web IDE settings: %w", err)
		}
	}
	if len(repository) != 0 {
		var customizations webIDECustomizations
		if err := json.Unmarshal(repository, &customizations); err != nil {
			return webIDECustomizations{}, fmt.Errorf("decode VS Code customizations: %w", err)
		}
		maps.Copy(result.Settings, customizations.Settings)
		result.Extensions = mergeWebIDEExtensions(result.Extensions, customizations.Extensions)
	}
	if result.Settings == nil {
		result.Settings = map[string]json.RawMessage{}
	}
	for i, extension := range result.Extensions {
		result.Extensions[i] = strings.TrimSpace(extension)
		if result.Extensions[i] == "" || len(result.Extensions[i]) > 255 || strings.ContainsAny(result.Extensions[i], " \t\r\n") {
			return webIDECustomizations{}, fmt.Errorf("VS Code extension %q is empty, too long, or contains whitespace", extension)
		}
	}
	return result, nil
}

func mergeWebIDEExtensions(defaults, repository []string) []string {
	result := make([]string, 0, len(defaults)+len(repository))
	positions := make(map[string]int, len(defaults)+len(repository))
	for _, extension := range append(append([]string(nil), defaults...), repository...) {
		extension = strings.TrimSpace(extension)
		identity := strings.ToLower(strings.SplitN(extension, "@", 2)[0])
		if position, exists := positions[identity]; exists {
			result[position] = extension
			continue
		}
		positions[identity] = len(result)
		result = append(result, extension)
	}
	return result
}
