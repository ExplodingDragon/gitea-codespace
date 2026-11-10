// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cgroup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseLimits(t *testing.T) {
	quota, period, unlimited, err := ParseCPUQuota([]byte("max 100000\n"))
	require.NoError(t, err)
	require.True(t, unlimited)
	require.Zero(t, quota)
	require.Equal(t, int64(100000), period)

	quota, period, unlimited, err = ParseCPUQuota([]byte("200000 100000\n"))
	require.NoError(t, err)
	require.False(t, unlimited)
	require.Equal(t, int64(200000), quota)
	require.Equal(t, int64(100000), period)
	_, _, _, err = ParseCPUQuota([]byte("invalid"))
	require.Error(t, err)

	got, err := ParseMemoryMax([]byte("max\n"))
	require.NoError(t, err)
	require.Zero(t, got)
	got, err = ParseMemoryMax([]byte("1073741824\n"))
	require.NoError(t, err)
	require.Equal(t, int64(1073741824), got)
	_, err = ParseMemoryMax([]byte("invalid"))
	require.Error(t, err)
}

func TestReader(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "memory.current"), []byte("42\n"), 0o600))
	reader := At(root)
	value, err := reader.ReadInt("memory.current")
	require.NoError(t, err)
	require.Equal(t, int64(42), value)
	_, err = reader.Read("../outside")
	require.Error(t, err)
}

func TestResolveMountPath(t *testing.T) {
	tests := []struct {
		name                 string
		processPath, root    string
		mountpoint, expected string
		err                  bool
	}{
		{name: "root mount", processPath: "/kubepods/pod", root: "/", mountpoint: "/sys/fs/cgroup", expected: "/sys/fs/cgroup/kubepods/pod"},
		{name: "namespace root", processPath: "/delegated", root: "/delegated", mountpoint: "/sys/fs/cgroup", expected: "/sys/fs/cgroup"},
		{name: "delegated child", processPath: "/delegated/child", root: "/delegated", mountpoint: "/sys/fs/cgroup", expected: "/sys/fs/cgroup/child"},
		{name: "outside hierarchy", processPath: "/other", root: "/delegated", mountpoint: "/sys/fs/cgroup", err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveMountPath(test.processPath, test.root, test.mountpoint)
			if test.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, got)
		})
	}
}
