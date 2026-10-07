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
func TestAgentPID1Supervision(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_AGENT_PROCESS") != "1" {
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

func TestAgentDockerSupervision(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_AGENT_DOCKER") != "1" {
		t.Skip("requires a dedicated Sysbox or Kata Runtime Pod")
	}
	require.NoError(t, os.MkdirAll("/run/codespace", 0o700))
	directory, err := os.MkdirTemp("/var/lib/codespace", "agent-docker-")
	require.NoError(t, err)
	// Sysbox retains its data-root mount until Pod teardown. The enclosing
	// Kubernetes test owns the disposable Pod/PVC and reclaims it there.
	daemon, err := StartDocker(t.Context(), directory, nil, os.Stderr)
	require.NoError(t, err)
	require.NoError(t, daemon.Close())
	select {
	case <-daemon.Done():
	default:
		t.Fatal("Docker shutdown returned before the daemon exited")
	}
}
