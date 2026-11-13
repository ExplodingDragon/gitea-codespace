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
	engine := &Engine{hostCgroup: &reader}
	oneCPU := 1
	require.NoError(t, engine.checkHostRequirements(devcontainer.HostRequirements{CPUs: &oneCPU, Memory: "256MiB"}, t.TempDir()))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cpu.max"), []byte("50000 100000\n"), 0o600))
	require.ErrorContains(t, engine.checkHostRequirements(devcontainer.HostRequirements{CPUs: &oneCPU}, t.TempDir()), "CPU quota")
	require.ErrorContains(t, engine.checkHostRequirements(devcontainer.HostRequirements{Memory: "1GiB"}, t.TempDir()), "memory")
}

func TestCheckHostRequirementsReportsUnavailableLimits(t *testing.T) {
	reader := cgroup.At(t.TempDir())
	engine := &Engine{hostCgroup: &reader}
	cpu := 1
	require.ErrorContains(t, engine.checkHostRequirements(devcontainer.HostRequirements{Memory: "256MiB"}, t.TempDir()), "inspect runtime memory")
	require.ErrorContains(t, engine.checkHostRequirements(devcontainer.HostRequirements{CPUs: &cpu}, t.TempDir()), "inspect runtime CPU limit")
}
