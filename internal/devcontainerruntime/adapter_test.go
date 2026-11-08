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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestCreateOptionsAddsCodespacePolicy(t *testing.T) {
	t.Parallel()

	request := Request{
		CodespaceUUID: "11111111-2222-4333-8444-555555555555",
		Workspace:     "/workspaces/project",
		GiteaURL:      "https://gitea.example.com",
		Source:        devcontainer.Source{Path: ".devcontainer/devcontainer.json"},
		DevContainer: Configuration{
			GiteaCLI: true,
			WebIDE: WebIDEConfiguration{
				Enabled: true, ArchiveURL: "https://editor.example/code-server-{arch}.tar.gz",
				Settings:   apiextensionsv1.JSON{Raw: []byte(`{"editor.tabSize":4}`)},
				Extensions: []string{"example.base@1.0.0"},
			},
			Features:        []InjectedFeature{{Reference: "ghcr.io/example/features/tool:1"}},
			LifecycleScript: "case \"$1\" in build) : ;; esac\n",
		},
		Cache: devcontainer.CacheOptions{
			BuildRegistry: "https://registry.example.com/cache",
			BuildScope:    "scope",
		},
		GiteaCustomCA: true,
		Native: NativeContainerConfiguration{
			CDIDeviceIDs: []string{"nvidia.com/gpu=GPU-1"}, Capabilities: []string{"SYS_PTRACE"}, ProvidesGPU: true,
			Devices:     []runtimeplatform.DeviceMapping{{Source: "/dev/example", Target: "/dev/example", Permissions: "rwm"}},
			Mounts:      []runtimeplatform.MountMapping{{Source: "/opt/example", Target: "/opt/example", ReadOnly: true}},
			Environment: map[string]string{"EXAMPLE_VISIBLE_DEVICES": "GPU-1"},
		},
	}
	options, err := buildCreateOptions(request)
	if err != nil {
		t.Fatalf("create options: %v", err)
	}
	if options.OwnerID != request.CodespaceUUID || options.AllowedPathRoot != request.Workspace {
		t.Fatalf("Codespace identity policy = owner %q, root %q", options.OwnerID, options.AllowedPathRoot)
	}
	require.Equal(t, []devcontainer.InjectedFeature{{Reference: "ghcr.io/example/features/tool:1", Origin: "environment template", Options: map[string]json.RawMessage{}}}, options.InjectedFeatures)
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: webide.CustomProgramPath, Target: webide.ContainerProgramPath, ReadOnly: true})
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: webide.UserDataSourcePath, Target: webide.ContainerUserDataPath})
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: webide.ExtensionsSourcePath, Target: webide.ContainerExtensionsPath})
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: webide.PlatformVSIXDirectory, Target: webide.ContainerVSIXDirectory, ReadOnly: true})
	if options.Labels["dev.gitea.codespace.uuid"] != request.CodespaceUUID {
		t.Fatalf("Codespace label = %#v", options.Labels)
	}
	if options.Cache.BuildRegistry != request.Cache.BuildRegistry || options.Cache.BuildScope != request.Cache.BuildScope {
		t.Fatalf("Codespace cache = %#v", options.Cache)
	}
	require.Equal(t, request.Native.CDIDeviceIDs, options.PlatformDevices.CDI)
	require.Equal(t, request.Native.Capabilities, options.PlatformDevices.Capabilities)
	require.True(t, options.PlatformDevices.ProvidesGPU)
	require.NotContains(t, options.ContainerEnv, "EXAMPLE_VISIBLE_DEVICES")
	require.Contains(t, options.AdditionalMounts, devcontainer.Mount{Type: "bind", Source: "/opt/example", Target: "/opt/example", ReadOnly: true})
	if options.ContainerEnv["CODESPACE_CA_BUNDLE"] != containerCABundlePath || options.ContainerEnv["NODE_EXTRA_CA_CERTS"] != containerCABundlePath || options.ContainerEnv["GITEA_CA_BUNDLE"] != containerCABundlePath || options.ContainerEnv["GITEA_INSTANCE_URL"] != request.GiteaURL || options.ContainerEnv["GITEA_CLI_NO_UPDATE_NOTIFIER"] != "1" {
		t.Fatalf("custom CA environment = %#v", options.ContainerEnv)
	}
	request.GiteaCustomCA = false
	options, err = buildCreateOptions(request)
	if err != nil {
		t.Fatalf("create options without custom CA: %v", err)
	}
	if len(options.ContainerEnv) != 4 || options.ContainerEnv["CODESPACE_CA_BUNDLE"] != containerCABundlePath || options.ContainerEnv["NODE_EXTRA_CA_CERTS"] != containerCABundlePath || options.ContainerEnv["GITEA_INSTANCE_URL"] != request.GiteaURL || options.ContainerEnv["GITEA_CLI_NO_UPDATE_NOTIFIER"] != "1" {
		t.Fatalf("system CA environment = %#v", options.ContainerEnv)
	}
}

func TestResolveWebIDECustomizations(t *testing.T) {
	t.Parallel()

	repository := json.RawMessage(`{
		"extensions": ["example.base@2.0.0", "example.repository"],
		"settings": {
			"editor.tabSize": 2,
			"files.autoSave": "afterDelay"
		}
	}`)

	configuration := WebIDEConfiguration{
		Settings:   apiextensionsv1.JSON{Raw: []byte(`{"editor.tabSize":4,"editor.wordWrap":"on"}`)},
		Extensions: []string{"example.base@1.0.0", "example.template"},
	}
	resolved, err := resolveWebIDECustomizations(configuration, repository)
	require.NoError(t, err)
	require.Equal(t, []string{"example.base@2.0.0", "example.template", "example.repository"}, resolved.Extensions)
	require.JSONEq(t, `2`, string(resolved.Settings["editor.tabSize"]))
	require.JSONEq(t, `"on"`, string(resolved.Settings["editor.wordWrap"]))
	require.JSONEq(t, `"afterDelay"`, string(resolved.Settings["files.autoSave"]))
}
