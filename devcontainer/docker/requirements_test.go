// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/cgroup"
	"github.com/stretchr/testify/require"
)

func TestCheckHostRequirementsUsesCgroupQuotaAndMemory(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(value), 0o600))
	}
	write("cpu.max", "100000 100000\n")
	write("memory.max", "536870912\n")
	reader := cgroup.At(root)
	oneCPU := 1
	require.NoError(t, checkHostRequirementsWithCgroup(devcontainer.HostRequirements{CPUs: &oneCPU, Memory: "256MiB"}, t.TempDir(), reader))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cpu.max"), []byte("50000 100000\n"), 0o600))
	require.ErrorContains(t, checkHostRequirementsWithCgroup(devcontainer.HostRequirements{CPUs: &oneCPU}, t.TempDir(), reader), "CPU quota")
	require.ErrorContains(t, checkHostRequirementsWithCgroup(devcontainer.HostRequirements{Memory: "1GiB"}, t.TempDir(), reader), "memory")
}

func TestCheckHostRequirementsRequiresMemoryCgroupWhenRequested(t *testing.T) {
	root := t.TempDir()
	reader := cgroup.At(root)
	require.ErrorContains(t, checkHostRequirementsWithCgroup(devcontainer.HostRequirements{Memory: "256MiB"}, t.TempDir(), reader), "inspect runtime memory")
}

func TestCheckHostRequirementsRequiresCPUQuotaWhenRequested(t *testing.T) {
	cpu := 1
	require.ErrorContains(t, checkHostRequirementsWithCgroup(devcontainer.HostRequirements{CPUs: &cpu}, t.TempDir(), cgroup.Reader{}), "inspect runtime CPU limit")
}
