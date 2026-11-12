// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/stretchr/testify/require"
)

func TestRequestRejectsIncompleteUSBIPMounts(t *testing.T) {
	err := (Request{
		Action:        "create",
		CodespaceUUID: "runtime",
		USBIP:         USBIPContainerConfiguration{Enabled: true, DeviceRoot: "/runtime/dev"},
	}).Validate()
	require.ErrorContains(t, err, "USB/IP device mounts are incomplete")
}

func TestUSBIPContainerMutatorPreservesRuntimeSettings(t *testing.T) {
	mutate := usbIPContainerMutator(USBIPContainerConfiguration{Enabled: true, DeviceRoot: "/runtime/dev", DevptsRoot: "/runtime/devpts"})
	host := &container.HostConfig{IpcMode: "private", ShmSize: 128 * 1024 * 1024, CapDrop: []string{"ALL"}}
	require.NoError(t, mutate(&container.Config{}, host))
	require.Equal(t, []string{"ALL"}, []string(host.CapDrop))
	require.Contains(t, []string(host.CapAdd), "NET_ADMIN")
	require.ElementsMatch(t, []string{"c 189:* rwm", "c 188:* rwm", "c 166:* rwm"}, host.DeviceCgroupRules)
	require.Len(t, host.Mounts, 3)
	require.Equal(t, mount.TypeBind, host.Mounts[0].Type)
	require.Equal(t, "/dev", host.Mounts[0].Target)
	require.True(t, host.Mounts[0].BindOptions.NonRecursive)
	require.Equal(t, "/dev/pts", host.Mounts[1].Target)
	require.Equal(t, "/dev/shm", host.Mounts[2].Target)
	require.EqualValues(t, 128*1024*1024, host.Mounts[2].TmpfsOptions.SizeBytes)
}

func TestUSBIPContainerMutatorRejectsDeviceMountConflict(t *testing.T) {
	mutate := usbIPContainerMutator(USBIPContainerConfiguration{Enabled: true, DeviceRoot: "/runtime/dev", DevptsRoot: "/runtime/devpts"})
	for name, test := range map[string]struct {
		config *container.Config
		host   *container.HostConfig
	}{
		"bind":   {config: &container.Config{}, host: &container.HostConfig{Binds: []string{"/tmp/dev:/dev"}}},
		"mount":  {config: &container.Config{}, host: &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeBind, Source: "/tmp/dev", Target: "/dev/"}}}},
		"volume": {config: &container.Config{Volumes: map[string]struct{}{"/dev": {}}}, host: &container.HostConfig{}},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, mutate(test.config, test.host), "platform ownership of /dev")
		})
	}
}
