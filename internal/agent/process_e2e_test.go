// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitea.dev/codespace/internal/cgroup"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
)

func TestRuntimeNetworkHasUsableMTU(t *testing.T) {
	interfaces := []net.Interface{
		{Flags: net.FlagUp | net.FlagLoopback, MTU: 65536},
		{Flags: 0, MTU: 1200},
		{Flags: net.FlagUp, MTU: 1500},
		{Flags: net.FlagUp, MTU: 1450},
	}
	require.Equal(t, 1450, runtimeNetworkMTU(interfaces))
	require.Zero(t, runtimeNetworkMTU(interfaces[:2]))
}

// Run this binary with unshare --pid --fork --mount-proc in the isolated test
// Runtime Pod. The ordinary Go test process must not act as a host subreaper.
func TestAgentE2EPID1Supervision(t *testing.T) {
	if os.Getenv("CODESPACE_E2E") != "1" {
		t.Skip("requires an isolated Runtime Pod PID namespace")
	}
	require.Equal(t, 1, os.Getpid())
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	stopped := filepath.Join(directory, "stopped")
	orphan := filepath.Join(directory, "orphan")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observed := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				observed <- ctx.Err()
				return
			case <-ticker.C:
				if _, err := os.Stat(ready); err == nil {
					data, err := os.ReadFile(orphan)
					pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
					if err != nil || parseErr != nil || pid <= 1 {
						continue
					}
					if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); errors.Is(err, os.ErrNotExist) {
						cancel()
						observed <- nil
						return
					}
				}
			}
		}
	}()
	// The short-lived intermediate shell leaves a child for PID 1 to reap.
	// TERM must reach the worker first, allowing its cleanup trap to complete.
	err := Supervise(ctx, "/bin/sh", []string{"-c", `trap 'printf stopped > "$2"; exit 0' TERM INT
sh -c 'sleep 0.01 & printf "%s" "$!" > "$1"' orphan "$3"
sleep 0.05
printf ready > "$1"
while :; do sleep 0.05; done`, "worker", ready, stopped, orphan}, os.Stdin, os.Stdout, os.Stderr)
	require.NoError(t, err)
	require.NoError(t, <-observed)
	data, err := os.ReadFile(stopped)
	require.NoError(t, err)
	require.Equal(t, "stopped", string(data))
}

func TestAgentE2EDockerSupervision(t *testing.T) {
	if os.Getenv("CODESPACE_E2E") != "1" {
		t.Skip("requires an isolated Runtime Pod")
	}
	require.NoError(t, os.MkdirAll("/run/codespace", 0o700))
	directory, err := os.MkdirTemp("/var/lib/codespace", "agent-docker-")
	require.NoError(t, err)
	// Sysbox retains its data-root mount until Pod teardown. The enclosing
	// Kubernetes test owns the disposable Pod/PVC and reclaims it there.
	_, hierarchy, err := cgroup.Discover()
	require.NoError(t, err)
	isolation := os.Getenv("CODESPACE_RUNTIME_ISOLATION")
	require.Contains(t, []string{"kata", "native", "sysbox"}, isolation)
	daemon, err := StartDocker(t.Context(), directory, "", isolation, hierarchy, defaultSystemCABundlePath, os.Stderr)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })
	docker, err := client.NewClientWithOpts(client.WithHost(DockerHost), client.WithAPIVersionNegotiation())
	require.NoError(t, err)
	defer func() { _ = docker.Close() }()
	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	mtu := strconv.Itoa(runtimeNetworkMTU(interfaces))
	const mtuOption = "com.docker.network.driver.mtu"
	for _, test := range []struct {
		name    string
		options map[string]string
		mtu     string
	}{
		{name: "bridge", mtu: mtu},
		{name: "compose-default", mtu: mtu},
		{name: "explicit-mtu", options: map[string]string{mtuOption: "1400"}, mtu: "1400"},
	} {
		if test.name != "bridge" {
			_, err := docker.NetworkCreate(t.Context(), test.name, network.CreateOptions{Driver: "bridge", Options: test.options})
			require.NoError(t, err)
		}
		info, err := docker.NetworkInspect(t.Context(), test.name, network.InspectOptions{})
		require.NoError(t, err)
		require.Equal(t, test.mtu, info.Options[mtuOption], test.name)
	}
	require.NoError(t, daemon.Close())
	select {
	case <-daemon.Done():
	default:
		t.Fatal("Docker shutdown returned before the daemon exited")
	}
}
