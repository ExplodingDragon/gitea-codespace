// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cgroup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/sys/mountinfo"
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

func TestResolveHierarchy(t *testing.T) {
	tests := []struct {
		name                 string
		processPath, root    string
		mountpoint, expected string
		err                  bool
	}{
		{name: "root mount", processPath: "/kubepods/pod", root: "/", mountpoint: "/sys/fs/cgroup", expected: "/sys/fs/cgroup/kubepods/pod"},
		{name: "namespace root", processPath: "/delegated", root: "/delegated", mountpoint: "/sys/fs/cgroup", expected: "/sys/fs/cgroup"},
		{name: "delegated child", processPath: "/delegated/child", root: "/delegated", mountpoint: "/sys/fs/cgroup", expected: "/sys/fs/cgroup/child"},
		{name: "nested agent", processPath: "/init", root: "/", mountpoint: "/sys/fs/cgroup", expected: "/sys/fs/cgroup/init"},
		{name: "sysbox init", processPath: "/init.scope", root: "/", mountpoint: "/sys/fs/cgroup", expected: "/sys/fs/cgroup/init.scope"},
		{name: "outside hierarchy", processPath: "/other", root: "/delegated", mountpoint: "/sys/fs/cgroup", err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current, hierarchy, err := resolveHierarchy([]byte("0::"+test.processPath+"\n"), []*mountinfo.Info{{Root: test.root, Mountpoint: test.mountpoint, FSType: "cgroup2"}})
			if test.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, At(test.expected), current)
			require.Equal(t, At(test.mountpoint), hierarchy)
		})
	}
}

func TestEnableNesting(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"cgroup.type": "domain\n", "cgroup.controllers": "cpu memory pids\n", "cgroup.procs": "42\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	}
	reader := At(root)
	require.NoError(t, reader.EnableNesting(t.Context()))
	data, err := os.ReadFile(filepath.Join(root, "init/cgroup.procs"))
	require.NoError(t, err)
	require.Equal(t, "42", string(data))
	data, err = reader.Read("cgroup.subtree_control")
	require.NoError(t, err)
	require.Equal(t, "+cpu +memory +pids", string(data))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.procs"), nil, 0o600))
	require.NoError(t, reader.EnableNesting(t.Context()))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, reader.EnableNesting(ctx), context.Canceled)
	require.NoError(t, os.WriteFile(filepath.Join(root, "cgroup.type"), []byte("domain threaded\n"), 0o600))
	require.ErrorContains(t, reader.EnableNesting(t.Context()), "recreate the Runtime Pod")
}
