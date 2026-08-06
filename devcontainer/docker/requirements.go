// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	dockerunits "github.com/docker/go-units"
	"golang.org/x/sys/unix"

	"gitea.dev/codespace/devcontainer"
)

func checkHostRequirements(requirements devcontainer.HostRequirements, workspace string) error {
	if requirements.CPUs > float64(runtime.NumCPU()) {
		return fmt.Errorf("dev container requires %g CPUs but the runtime provides %d", requirements.CPUs, runtime.NumCPU())
	}
	if strings.TrimSpace(requirements.Memory) != "" {
		required, err := dockerunits.RAMInBytes(requirements.Memory)
		if err != nil || required <= 0 {
			return fmt.Errorf("dev container hostRequirements.memory %q is invalid", requirements.Memory)
		}
		var available uint64
		if content, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil && strings.TrimSpace(string(content)) != "max" {
			available, _ = strconv.ParseUint(strings.TrimSpace(string(content)), 10, 64)
		}
		if available == 0 {
			var info unix.Sysinfo_t
			if err := unix.Sysinfo(&info); err != nil {
				return fmt.Errorf("inspect runtime memory: %w", err)
			}
			available = info.Totalram * uint64(info.Unit)
		}
		if uint64(required) > available {
			return fmt.Errorf("dev container requires %s memory but the runtime provides %s", requirements.Memory, dockerunits.BytesSize(float64(available)))
		}
	}
	if strings.TrimSpace(requirements.Storage) != "" {
		required, err := dockerunits.RAMInBytes(requirements.Storage)
		if err != nil || required <= 0 {
			return fmt.Errorf("dev container hostRequirements.storage %q is invalid", requirements.Storage)
		}
		var info unix.Statfs_t
		if err := unix.Statfs(workspace, &info); err != nil {
			return fmt.Errorf("inspect runtime storage: %w", err)
		}
		available := info.Bavail * uint64(info.Bsize)
		if uint64(required) > available {
			return fmt.Errorf("dev container requires %s storage but the runtime provides %s", requirements.Storage, dockerunits.BytesSize(float64(available)))
		}
	}
	return nil
}

func (e *Engine) resolveGPURequest(ctx context.Context, requirement []byte) (bool, error) {
	value := strings.TrimSpace(string(requirement))
	if value == "" || value == "null" || value == "false" {
		return false, nil
	}
	info, err := e.client.Info(ctx)
	if err != nil {
		return false, fmt.Errorf("inspect Docker GPU runtime: %w", err)
	}
	_, available := info.Runtimes["nvidia"]
	if !available && value != `"optional"` {
		return false, fmt.Errorf("dev container requires a GPU but the Docker nvidia runtime is unavailable")
	}
	return available, nil
}
