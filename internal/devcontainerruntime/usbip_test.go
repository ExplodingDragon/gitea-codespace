// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"strings"
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
	host := &container.HostConfig{
		IpcMode:           "private",
		ShmSize:           128 * 1024 * 1024,
		CapAdd:            []string{"SYS_PTRACE"},
		CapDrop:           []string{"ALL"},
		DeviceCgroupRules: []string{"c 189:* rwm", "b 8:* rw"},
		NetworkMode:       "bridge",
	}
	require.NoError(t, mutate(&container.Config{}, host))
	require.Equal(t, []string{"ALL"}, []string(host.CapDrop))
	require.Equal(t, []string{"SYS_PTRACE", "NET_ADMIN"}, []string(host.CapAdd))
	require.Contains(t, host.DeviceCgroupRules, "b 8:* rw")
	require.Contains(t, host.DeviceCgroupRules, "c 89:* rwm")
	require.Equal(t, "c 189:* rwm", host.DeviceCgroupRules[0])
	require.NotContains(t, host.DeviceCgroupRules[1:], "c 189:* rwm")
	require.Equal(t, container.NetworkMode("bridge"), host.NetworkMode)
	require.False(t, host.Privileged)
	require.Len(t, host.Mounts, 3)
	require.Equal(t, mount.TypeBind, host.Mounts[0].Type)
	require.Equal(t, "/dev", host.Mounts[0].Target)
	require.True(t, host.Mounts[0].BindOptions.NonRecursive)
	require.Equal(t, "/dev/pts", host.Mounts[1].Target)
	require.Equal(t, "/dev/shm", host.Mounts[2].Target)
	require.EqualValues(t, 128*1024*1024, host.Mounts[2].TmpfsOptions.SizeBytes)
}

func TestUSBIPDeviceCgroupRules(t *testing.T) {
	for _, test := range []struct {
		name    string
		devices string
		dynamic []string
	}{
		{name: "fixed classes", devices: "Character devices:\n  1 mem\nBlock devices:\n 240 hidraw\n 241 gpiochip\n"},
		{name: "registered dynamic classes", devices: "Character devices:\n 243 hidraw\n 254 gpiochip\nBlock devices:\n  8 sd\n", dynamic: []string{"c 243:* rwm", "c 254:* rwm"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rules, err := usbIPDeviceCgroupRules(strings.NewReader(test.devices))
			require.NoError(t, err)
			expected := append([]string{"c 189:* rwm", "c 188:* rwm", "c 166:* rwm", "c 89:* rwm", "c 153:* rwm", "c 180:* rwm"}, test.dynamic...)
			require.ElementsMatch(t, expected, rules)
		})
	}
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
