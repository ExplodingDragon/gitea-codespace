// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/codespace/internal/runtimeplatform"
	oci "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"tags.cncf.io/container-device-interface/pkg/cdi"
	cditypes "tags.cncf.io/container-device-interface/specs-go"
)

func TestPrepareNativeDevicesManualMappings(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, os.Symlink("/dev/null", filepath.Join(directory, "null")))
	require.NoError(t, os.Symlink("/dev/zero", filepath.Join(directory, "zero")))
	t.Setenv("DEVICE_RUNTIME", "current")
	configuration := &runtimeplatform.NativeConfiguration{
		DevContainerCapAdd: []corev1.Capability{"SYS_PTRACE"}, AdditionalGroups: []uint32{44, 44, 109},
		DevicePlugins: []runtimeplatform.DevicePluginRequest{{
			ResourceName: "example.com/device", Count: 1, ProvidesGPU: true,
			HostAccess: runtimeplatform.HostAccess{
				Devices:            []runtimeplatform.DeviceMapping{{Source: directory, Target: "/dev/example", Permissions: "rw"}},
				Mounts:             []runtimeplatform.MountMapping{{SourceScope: "runtime", Source: directory, Target: "/opt/example", ReadOnly: true}},
				ForwardEnvironment: []string{"DEVICE_RUNTIME"},
			},
		}},
	}
	output := t.TempDir()
	prepared, err := prepareNativeDevices(configuration, nil, output)
	require.NoError(t, err)
	require.True(t, prepared.DevContainer.ProvidesGPU)
	require.Equal(t, []string{"SYS_PTRACE"}, prepared.DevContainer.Capabilities)
	require.Equal(t, []uint32{44, 109}, prepared.DevContainer.AdditionalGroups)
	cache, err := cdi.NewCache(cdi.WithSpecDirs(output), cdi.WithAutoRefresh(false))
	require.NoError(t, err)
	spec := &oci.Spec{Process: &oci.Process{}}
	_, err = cache.InjectDevices(spec, nativeCDIDevice)
	require.NoError(t, err)
	require.Equal(t, "0.7.0", cache.GetDevice(nativeCDIDevice).GetSpec().Version)
	require.Len(t, spec.Linux.Devices, 2)
	require.Equal(t, "/dev/example/null", spec.Linux.Devices[0].Path)
	require.Equal(t, directory, spec.Mounts[0].Source)
	require.Contains(t, spec.Mounts[0].Options, "ro")
	require.Equal(t, []uint32{44, 109}, spec.Process.User.AdditionalGids)
	require.Equal(t, []string{"DEVICE_RUNTIME=current"}, spec.Process.Env)
	configuration.DevicePlugins[0].HostAccess.Devices = append(configuration.DevicePlugins[0].HostAccess.Devices,
		runtimeplatform.DeviceMapping{Source: "/dev/zero", Target: "/dev/example/null", Permissions: "rw"})
	_, err = prepareNativeDevices(configuration, nil, output)
	require.ErrorContains(t, err, "conflicting native device target")
}

func TestNativeCDISpecUsesStableAllocation(t *testing.T) {
	sourceDirectory, outputDirectory := t.TempDir(), t.TempDir()
	source, err := cdi.NewCache(cdi.WithSpecDirs(sourceDirectory), cdi.WithAutoRefresh(false))
	require.NoError(t, err)
	require.NoError(t, source.WriteSpec(&cditypes.Spec{
		Version: cditypes.CurrentVersion, Kind: "example.com/device",
		ContainerEdits: cditypes.ContainerEdits{Env: []string{"COMMON=spec", "OVERRIDE=spec"}},
		Devices: []cditypes.Device{
			{Name: "first", ContainerEdits: cditypes.ContainerEdits{Env: []string{"PHYSICAL=first", "OVERRIDE=device-first"}}},
			{Name: "second", ContainerEdits: cditypes.ContainerEdits{Env: []string{"PHYSICAL=second", "OVERRIDE=device-second"}}},
			{Name: "other", ContainerEdits: cditypes.ContainerEdits{Env: []string{"OTHER=1"}}},
		},
	}, "devices.json"))
	require.NoError(t, os.WriteFile(sourceDirectory+"/unrelated.json", []byte(`{"cdiVersion":"invalid"}`), 0o600))
	configuration := &runtimeplatform.NativeConfiguration{DevicePlugins: []runtimeplatform.DevicePluginRequest{{
		ResourceName: "example.com/device", Count: 2,
		CDI:        &runtimeplatform.CDIDevicePropagation{Kind: "example.com/device", Selection: "environment", DeviceIDEnvironment: "DEVICE_IDS"},
		HostAccess: runtimeplatform.HostAccess{ForwardEnvironment: []string{"OVERRIDE"}},
	}}}
	for _, physical := range []string{"first", "second"} {
		t.Setenv("DEVICE_IDS", "example.com/device="+physical+","+physical)
		t.Setenv("OVERRIDE", "forwarded-"+physical)
		prepared, err := prepareNativeDevices(configuration, []string{sourceDirectory}, outputDirectory)
		require.NoError(t, err)
		require.Equal(t, []string{nativeCDIDevice}, prepared.DevContainer.CDIDeviceIDs)
		require.Equal(t, map[string]string{"COMMON": "spec", "PHYSICAL": physical, "OVERRIDE": "forwarded-" + physical}, prepared.DevContainer.Environment)
		filtered, err := cdi.NewCache(cdi.WithSpecDirs(outputDirectory), cdi.WithAutoRefresh(false))
		require.NoError(t, err)
		spec := &oci.Spec{Process: &oci.Process{}}
		_, err = filtered.InjectDevices(spec, nativeCDIDevice)
		require.NoError(t, err)
		require.Equal(t, "0.3.0", filtered.GetDevice(nativeCDIDevice).GetSpec().Version)
		require.ElementsMatch(t, []string{"COMMON=spec", "PHYSICAL=" + physical, "OVERRIDE=forwarded-" + physical}, spec.Process.Env)
	}
	for _, ids := range []string{"", "all", "other.example/device=first", "missing"} {
		t.Setenv("DEVICE_IDS", ids)
		_, err := prepareNativeDevices(configuration, []string{sourceDirectory}, outputDirectory)
		require.Error(t, err, ids)
	}
}

func TestNativeCDISelectionByRuntimeNodes(t *testing.T) {
	directory, sourceDirectory, outputDirectory := t.TempDir(), t.TempDir(), t.TempDir()
	devicePath := filepath.Join(directory, "card0")
	require.NoError(t, os.Symlink("/dev/null", devicePath))
	source, err := cdi.NewCache(cdi.WithSpecDirs(sourceDirectory), cdi.WithAutoRefresh(false))
	require.NoError(t, err)
	definition := &cditypes.Spec{Version: cditypes.CurrentVersion, Kind: "example.com/device", Devices: []cditypes.Device{
		{Name: "card0", ContainerEdits: cditypes.ContainerEdits{DeviceNodes: []*cditypes.DeviceNode{{Path: devicePath, HostPath: "/node/card0", Type: "c", Major: 1, Minor: 3}},
			Mounts: []*cditypes.Mount{
				{HostPath: "/node/driver", ContainerPath: directory, Options: []string{"bind", "ro"}},
				{HostPath: "tmpfs", ContainerPath: "/run/device", Type: "tmpfs", Options: []string{"nosuid"}},
			}}},
		{Name: "card1", ContainerEdits: cditypes.ContainerEdits{DeviceNodes: []*cditypes.DeviceNode{{Path: filepath.Join(directory, "card1")}}}},
	}}
	definition.Devices = append(definition.Devices, cditypes.Device{Name: "all", ContainerEdits: definition.Devices[0].ContainerEdits})
	require.NoError(t, source.WriteSpec(definition, "devices.json"))
	configuration := &runtimeplatform.NativeConfiguration{DevicePlugins: []runtimeplatform.DevicePluginRequest{{
		ResourceName: "example.com/device", Count: 3,
		CDI: &runtimeplatform.CDIDevicePropagation{Kind: "example.com/device", Selection: "deviceNodes", DeviceNodePaths: []string{filepath.Join(directory, "card*")}},
	}}}
	prepared, err := prepareNativeDevices(configuration, []string{sourceDirectory}, outputDirectory)
	require.NoError(t, err)
	filtered, err := cdi.NewCache(cdi.WithSpecDirs(prepared.CDISpecDirectory), cdi.WithAutoRefresh(false))
	require.NoError(t, err)
	device := filtered.GetDevice(nativeCDIDevice)
	require.Len(t, device.ContainerEdits.DeviceNodes, 1)
	require.Equal(t, devicePath, device.ContainerEdits.DeviceNodes[0].HostPath)
	require.Equal(t, directory, device.ContainerEdits.Mounts[0].HostPath)
	require.Equal(t, "tmpfs", device.ContainerEdits.Mounts[1].HostPath)
	definition.Devices = append(definition.Devices, cditypes.Device{Name: "alias", ContainerEdits: definition.Devices[0].ContainerEdits})
	require.NoError(t, source.WriteSpec(definition, "devices.json"))
	_, err = prepareNativeDevices(configuration, []string{sourceDirectory}, outputDirectory)
	require.ErrorContains(t, err, "ambiguously matches")
	definition.Devices = definition.Devices[:1]
	definition.Devices[0].ContainerEdits.Hooks = []*cditypes.Hook{{HookName: "createRuntime", Path: filepath.Join(directory, "missing-hook")}}
	require.NoError(t, source.WriteSpec(definition, "devices.json"))
	_, err = prepareNativeDevices(configuration, []string{sourceDirectory}, outputDirectory)
	require.ErrorContains(t, err, "CDI hook")
}
