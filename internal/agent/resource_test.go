// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.dev/codespace/internal/cgroup"
	"github.com/stretchr/testify/require"
)

func TestResourceSamplerIndependentMetricsAndCPUBaseline(t *testing.T) {
	root := t.TempDir()
	volume := filepath.Join(root, "volume")
	require.NoError(t, os.Mkdir(volume, 0o700))
	write := func(name, value string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(value), 0o600))
	}
	write("cpu.stat", "usage_usec 0\n")
	write("cpu.max", "100000 100000\n")
	write("memory.current", "100\n")
	write("memory.stat", "inactive_file 20\n")
	write("memory.max", "200\n")
	sampler := &ResourceSampler{cgroup: cgroup.At(root), volume: volume}

	first := sampler.Sample()
	require.NotNil(t, first)
	require.Nil(t, first.Cpu, "CPU requires a baseline")
	require.Equal(t, int64(80), first.Memory.UsedBytes)
	require.Equal(t, int64(200), first.Memory.LimitBytes)
	require.NotNil(t, first.Disk)

	require.Greater(t, first.Disk.LimitBytes, int64(0))
	require.GreaterOrEqual(t, first.Disk.UsedBytes, int64(0))
	require.LessOrEqual(t, first.Disk.UsedBytes, first.Disk.LimitBytes)

	write("cpu.stat", "usage_usec 2000\n")
	cpu, err := sampler.sampleCPU(sampler.lastClock.Add(10 * time.Millisecond))
	require.NoError(t, err)
	require.NotNil(t, cpu)
	require.EqualValues(t, 200, cpu.UsedMillicores)
	require.EqualValues(t, 1000, cpu.LimitMillicores)
}

func TestResourceSamplerOmitsMetricWithUnreadableLimit(t *testing.T) {
	root := t.TempDir()
	volume := filepath.Join(root, "volume")
	require.NoError(t, os.Mkdir(volume, 0o700))
	write := func(name, value string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(value), 0o600))
	}
	write("cpu.stat", "usage_usec 1000\n")
	write("cpu.max", "100000 100000\n")
	write("memory.current", "100\n")
	write("memory.stat", "inactive_file 20\n")
	write("memory.max", "200\n")
	sampler := &ResourceSampler{cgroup: cgroup.At(root), volume: volume}
	first := sampler.Sample()
	require.NotNil(t, first.Memory)
	require.NotNil(t, first.Disk)
	require.NoError(t, os.Remove(filepath.Join(root, "cpu.max")))
	require.NoError(t, os.Remove(filepath.Join(root, "memory.max")))
	second := sampler.Sample()
	require.Nil(t, second.Cpu)
	require.Nil(t, second.Memory)
	require.NotNil(t, second.Disk)
}
