// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"encoding/json"
	"testing"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/runtimeplatform"
	"gitea.dev/codespace/internal/webide"
	"github.com/stretchr/testify/require"
)

func TestCreateOptionsAddsCodespacePolicy(t *testing.T) {
	request := Request{
		CodespaceUUID: "11111111-2222-4333-8444-555555555555", Workspace: "/workspaces/project", GiteaURL: "https://gitea.example.com",
		Source: devcontainer.Source{Path: ".devcontainer/devcontainer.json"},
		DevContainer: Configuration{
			GiteaCLI: true, WebIDE: WebIDEConfiguration{Provider: "vscode", Image: "ghcr.io/example/web-ide@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			Features: []InjectedFeature{{Reference: "ghcr.io/example/features/tool:1"}}, LifecycleScript: "case \"$1\" in build) : ;; esac\n",
		},
		Cache: devcontainer.CacheOptions{BuildRegistry: "https://registry.example.com/cache", BuildScope: "scope"},
		Native: NativeContainerConfiguration{
			CDIDeviceIDs: []string{"nvidia.com/gpu=GPU-1"}, Capabilities: []string{"SYS_PTRACE"}, ProvidesGPU: true,
			Devices: []runtimeplatform.DeviceMapping{{Source: "/dev/example", Target: "/dev/example", Permissions: "rwm"}},
			Mounts:  []runtimeplatform.MountMapping{{Source: "/opt/example", Target: "/opt/example", ReadOnly: true}},
		},
	}
	options, err := buildCreateOptions(request)
	require.NoError(t, err)
	require.Equal(t, request.CodespaceUUID, options.OwnerID)
	require.Equal(t, request.Workspace, options.AllowedPathRoot)
	require.Equal(t, []devcontainer.InjectedFeature{{Reference: "ghcr.io/example/features/tool:1", Origin: "environment template", Options: map[string]json.RawMessage{}}}, options.InjectedFeatures)
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: webide.ProgramRootPath, Target: webide.ContainerProgramPath, ReadOnly: true})
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: webide.ProviderDataPath("vscode"), Target: webide.ContainerDataPath})
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: "/opt/example", Target: "/opt/example", ReadOnly: true})
	require.Equal(t, request.Native.CDIDeviceIDs, options.PlatformDevices.CDI)
	require.Equal(t, "1", options.ContainerEnv["GITEA_CLI_NO_UPDATE_NOTIFIER"])
	require.Equal(t, containerCABundlePath, options.PlatformEnvironment["SSL_CERT_FILE"])
	require.Equal(t, containerCABundlePath, options.PlatformEnvironment["NODE_EXTRA_CA_CERTS"])
}

func TestCreateOptionsSkipsWebIDEMountsWhenDisabled(t *testing.T) {
	options, err := buildCreateOptions(Request{DevContainer: Configuration{}})
	require.NoError(t, err)
	for _, mount := range options.AdditionalMounts {
		require.NotEqual(t, webide.ContainerProgramPath, mount.Target)
		require.NotEqual(t, webide.ContainerDataPath, mount.Target)
	}
}
