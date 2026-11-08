// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"os"
	"testing"

	"gitea.dev/codespace/internal/runtimeplatform"
	oci "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"tags.cncf.io/container-device-interface/pkg/cdi"
	cditypes "tags.cncf.io/container-device-interface/specs-go"
)

func TestPrepareNativeDevicesForwardsAllocatedConfiguration(t *testing.T) {
	prepared, err := PrepareNativeDevices(&runtimeplatform.NativeConfiguration{
		DevContainerCapAdd: []corev1.Capability{"SYS_PTRACE"},
		DevicePlugins: []runtimeplatform.DevicePluginRequest{{
			ResourceName: "example.com/device", Count: 1, ProvidesGPU: true,
			HostAccess: runtimeplatform.HostAccess{
				Devices: []runtimeplatform.DeviceMapping{{Source: "/dev/example", Target: "/dev/example", Permissions: "rwm"}},
			},
		}},
	})
	require.NoError(t, err)
	require.True(t, prepared.DevContainer.ProvidesGPU)
	require.Equal(t, []string{"SYS_PTRACE"}, prepared.DevContainer.Capabilities)
	require.Empty(t, prepared.CDISpecDirectory)
}

func TestNativeCDISpecUsesStableAliases(t *testing.T) {
	sourceDirectory, outputDirectory := t.TempDir(), t.TempDir()
	source, err := cdi.NewCache(cdi.WithSpecDirs(sourceDirectory), cdi.WithAutoRefresh(false))
	require.NoError(t, err)
	require.NoError(t, source.WriteSpec(&cditypes.Spec{
		Version: cditypes.CurrentVersion, Kind: "example.com/device",
		Devices: []cditypes.Device{
			{Name: "first", ContainerEdits: cditypes.ContainerEdits{Env: []string{"PHYSICAL=first"}}},
			{Name: "second", ContainerEdits: cditypes.ContainerEdits{Env: []string{"PHYSICAL=second"}}},
			{Name: "other", ContainerEdits: cditypes.ContainerEdits{Env: []string{"OTHER=1"}}},
		},
	}, "devices.json"))
	require.NoError(t, os.WriteFile(sourceDirectory+"/unrelated.json", []byte(`{"cdiVersion":"invalid"}`), 0o600))
	alias := "example.com/device=codespace-0-0"
	require.NoError(t, writeNativeCDISpecs(
		[]allocatedCDIDevice{{physical: "example.com/device=first", alias: alias}},
		map[string]string{"EXAMPLE_VISIBLE_DEVICES": "first"}, []string{sourceDirectory}, outputDirectory,
	))

	filtered, err := cdi.NewCache(cdi.WithSpecDirs(outputDirectory), cdi.WithAutoRefresh(false))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{alias, "codespace.gitea.dev/environment=current"}, filtered.ListDevices())
	require.Nil(t, filtered.GetDevice("example.com/device=first"))
	containerSpec := &oci.Spec{Process: &oci.Process{}}
	unresolved, err := filtered.InjectDevices(containerSpec, alias, "codespace.gitea.dev/environment=current")
	require.NoError(t, err)
	require.Empty(t, unresolved)
	require.ElementsMatch(t, []string{"PHYSICAL=first", "EXAMPLE_VISIBLE_DEVICES=first"}, containerSpec.Process.Env)

	require.NoError(t, writeNativeCDISpecs(
		[]allocatedCDIDevice{{physical: "example.com/device=second", alias: alias}},
		map[string]string{"EXAMPLE_VISIBLE_DEVICES": "second"}, []string{sourceDirectory}, outputDirectory,
	))
	filtered, err = cdi.NewCache(cdi.WithSpecDirs(outputDirectory), cdi.WithAutoRefresh(false))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{alias, "codespace.gitea.dev/environment=current"}, filtered.ListDevices())
	require.Nil(t, filtered.GetDevice("example.com/device=second"))
	containerSpec = &oci.Spec{Process: &oci.Process{}}
	unresolved, err = filtered.InjectDevices(containerSpec, alias, "codespace.gitea.dev/environment=current")
	require.NoError(t, err)
	require.Empty(t, unresolved)
	require.ElementsMatch(t, []string{"PHYSICAL=second", "EXAMPLE_VISIBLE_DEVICES=second"}, containerSpec.Process.Env)
}
