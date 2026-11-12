// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"gitea.dev/codespace/internal/runtimeplatform"
	"gitea.dev/codespace/internal/webide"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

func TestCreateOptionsAddsCodespacePolicy(t *testing.T) {
	t.Parallel()
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
	asset := filepath.Join(t.TempDir(), "gt")
	require.NoError(t, os.WriteFile(asset, []byte("binary"), 0o555))
	options, err := buildCreateOptions(request, asset)
	require.NoError(t, err)
	require.Equal(t, request.CodespaceUUID, options.OwnerID)
	require.Equal(t, request.Workspace, options.AllowedPathRoot)
	require.Equal(t, []devcontainer.InjectedFeature{{Reference: "ghcr.io/example/features/tool:1", Origin: "environment template", Options: map[string]json.RawMessage{}}}, options.InjectedFeatures)
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: webide.ProgramRootPath, Target: webide.ContainerProgramPath, ReadOnly: true})
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: webide.ProviderDataPath("vscode"), Target: webide.ContainerDataPath})
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: "/opt/example", Target: "/opt/example", ReadOnly: true})
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{
		Type: "bind", Source: filepath.Dir(runtimeendpoint.AgentSocketPath), Target: filepath.Dir(runtimeendpoint.ContainerSocketPath), ReadOnly: true,
	})
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{
		Type: "bind", Source: filepath.Dir(RuntimeEnvironmentPath), Target: filepath.Dir(containerEnvironmentPath), ReadOnly: true,
	})
	require.Equal(t, request.Native.CDIDeviceIDs, options.PlatformDevices.CDI)
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: asset, Target: platformGiteaCLIPath, ReadOnly: true})
	require.Empty(t, options.ContainerEnv, "platform environment is applied after image builds")
	config := &container.Config{Env: []string{"GITEA_INSTANCE_URL=old", "PROJECT=value=with=equals"}}
	require.NoError(t, options.MutateContainer(config, &container.HostConfig{}))
	require.Equal(t, []string{"GITEA_CLI_NO_UPDATE_NOTIFIER=1", "GITEA_INSTANCE_URL=" + request.GiteaURL, "PROJECT=value=with=equals"}, config.Env)
	require.Equal(t, containerCABundlePath, options.PlatformEnvironment["SSL_CERT_FILE"])
	require.Equal(t, containerCABundlePath, options.PlatformEnvironment["NODE_EXTRA_CA_CERTS"])
}

func TestCreateOptionsSkipsWebIDEMountsWhenDisabled(t *testing.T) {
	t.Parallel()
	options, err := buildCreateOptions(Request{GiteaURL: "https://another.example.com"}, "")
	require.NoError(t, err)
	require.Empty(t, options.ContainerEnv)
	config := &container.Config{Env: []string{"PROJECT=kept"}}
	require.NoError(t, options.MutateContainer(config, &container.HostConfig{}))
	require.Equal(t, []string{"GITEA_INSTANCE_URL=https://another.example.com", "PROJECT=kept"}, config.Env)
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{
		Type: "bind", Source: filepath.Dir(RuntimeEnvironmentPath), Target: filepath.Dir(containerEnvironmentPath), ReadOnly: true,
	})
	for _, mount := range options.AdditionalMounts {
		require.NotEqual(t, webide.ContainerProgramPath, mount.Target)
		require.NotEqual(t, webide.ContainerDataPath, mount.Target)
		require.NotEqual(t, platformGiteaCLIPath, mount.Target)
	}
}

func TestCreateOptionsRequiresExecutableGiteaCLI(t *testing.T) {
	t.Parallel()
	request := Request{DevContainer: Configuration{GiteaCLI: true}}
	asset := filepath.Join(t.TempDir(), "gt")
	_, err := buildCreateOptions(request, asset)
	require.ErrorContains(t, err, "inspect platform Gitea CLI asset")
	require.NoError(t, os.WriteFile(asset, []byte("binary"), 0o600))
	_, err = buildCreateOptions(request, asset)
	require.ErrorContains(t, err, "executable regular file")
}
