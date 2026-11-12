// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package devcontainerruntime translates Manager Runtime configuration into
// Dev Container engine operations and Agent endpoint state.
package devcontainerruntime

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"gitea.dev/codespace/internal/webide"
	"github.com/docker/docker/api/types/container"
)

const containerCABundlePath = "/var/lib/gitea-codespace/ca/bundle.pem"

var runtimeMounts = [...]struct {
	source string
	target string
}{
	{source: "/run/codespace/gitea-token", target: "/var/lib/gitea-codespace/gitea-token"},
	{source: "/run/codespace/git", target: "/var/lib/gitea-codespace/git"},
	{source: "/run/codespace/ca", target: "/var/lib/gitea-codespace/ca"},
	{source: "/usr/local/bin/gitea-codespace", target: ContainerRuntimeBinary},
}

func buildCreateOptions(request Request, giteaCLIPath string) (containerdocker.CreateOptions, error) {
	mounts := make([]devcontainer.Mount, 0, len(runtimeMounts)+4)
	for _, item := range runtimeMounts {
		if _, err := os.Stat(item.source); err == nil {
			mounts = append(mounts, devcontainer.Mount{Type: "bind", Source: item.source, Target: item.target, ReadOnly: true})
		} else if !os.IsNotExist(err) {
			return containerdocker.CreateOptions{}, fmt.Errorf("inspect runtime mount %s: %w", item.source, err)
		}
	}
	// Directory mounts follow atomically replaced credentials and recreated sockets.
	mounts = append(mounts,
		devcontainer.Mount{Type: "bind", Source: filepath.Dir(runtimeendpoint.AgentSocketPath), Target: filepath.Dir(runtimeendpoint.ContainerSocketPath), ReadOnly: true},
		devcontainer.Mount{Type: "bind", Source: filepath.Dir(RuntimeEnvironmentPath), Target: filepath.Dir(containerEnvironmentPath), ReadOnly: true},
	)
	if request.DevContainer.GiteaCLI {
		info, err := os.Lstat(giteaCLIPath)
		if err != nil {
			return containerdocker.CreateOptions{}, fmt.Errorf("inspect platform Gitea CLI asset: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return containerdocker.CreateOptions{}, fmt.Errorf("platform Gitea CLI asset %s must be an executable regular file", giteaCLIPath)
		}
		mounts = append(mounts, devcontainer.Mount{Type: "bind", Source: giteaCLIPath, Target: platformGiteaCLIPath, ReadOnly: true})
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
		"GITEA_INSTANCE_URL": request.GiteaURL,
	}
	if request.DevContainer.GiteaCLI {
		containerEnvironment["GITEA_CLI_NO_UPDATE_NOTIFIER"] = "1"
	}
	// The CA bundle is mounted at runtime, after Feature image builds finish.
	platformEnvironment := map[string]string{
		"NODE_EXTRA_CA_CERTS": containerCABundlePath,
		"SSL_CERT_FILE":       containerCABundlePath,
	}
	for _, name := range devcontainer.ProxyVariableNames() {
		if value := request.LocalEnvironment[name]; value != "" {
			platformEnvironment[name] = value
		}
	}
	deviceMappings := make([]containerdocker.DeviceMapping, 0, len(request.Native.Devices))
	for _, mapping := range request.Native.Devices {
		deviceMappings = append(deviceMappings, containerdocker.DeviceMapping{Source: mapping.Source, Target: mapping.Target, Permissions: mapping.Permissions})
	}
	mutateUSBIP := usbIPContainerMutator(request.USBIP)
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
		Labels:              map[string]string{"dev.gitea.codespace.uuid": request.CodespaceUUID},
		PlatformDevices: containerdocker.PlatformDevices{
			CDI: request.Native.CDIDeviceIDs, Mappings: deviceMappings, Capabilities: request.Native.Capabilities, ProvidesGPU: request.Native.ProvidesGPU,
		},
		MutateContainer: func(config *container.Config, host *container.HostConfig) error {
			// Runtime-only values must not enter Feature builds or their cache keys.
			environment := make(map[string]string, len(config.Env)+len(containerEnvironment))
			for _, item := range config.Env {
				if name, value, ok := strings.Cut(item, "="); ok {
					environment[name] = value
				}
			}
			maps.Copy(environment, containerEnvironment)
			config.Env = devcontainer.EnvironmentList(environment)
			if mutateUSBIP != nil {
				return mutateUSBIP(config, host)
			}
			return nil
		},
	}, nil
}
