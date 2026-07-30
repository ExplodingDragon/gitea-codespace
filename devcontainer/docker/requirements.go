// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
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
	if requirements.CPUs < 0 {
		return fmt.Errorf("Dev Container hostRequirements.cpus must not be negative")
	}
	if requirements.CPUs > float64(runtime.NumCPU()) {
		return fmt.Errorf("Dev Container requires %.2f CPUs but the runtime provides %d", requirements.CPUs, runtime.NumCPU())
	}
	if strings.TrimSpace(requirements.Memory) != "" {
		required, err := dockerunits.RAMInBytes(requirements.Memory)
		if err != nil || required <= 0 {
			return fmt.Errorf("Dev Container hostRequirements.memory %q is invalid", requirements.Memory)
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
			return fmt.Errorf("Dev Container requires %s memory but the runtime provides %s", requirements.Memory, dockerunits.BytesSize(float64(available)))
		}
	}
	if strings.TrimSpace(requirements.Storage) != "" {
		required, err := dockerunits.RAMInBytes(requirements.Storage)
		if err != nil || required <= 0 {
			return fmt.Errorf("Dev Container hostRequirements.storage %q is invalid", requirements.Storage)
		}
		var info unix.Statfs_t
		if err := unix.Statfs(workspace, &info); err != nil {
			return fmt.Errorf("inspect runtime storage: %w", err)
		}
		available := info.Bavail * uint64(info.Bsize)
		if uint64(required) > available {
			return fmt.Errorf("Dev Container requires %s storage but the runtime provides %s", requirements.Storage, dockerunits.BytesSize(float64(available)))
		}
	}
	gpu := strings.TrimSpace(string(requirements.GPU))
	if gpu != "" && gpu != "null" && gpu != "false" && gpu != `"optional"` {
		return fmt.Errorf("Dev Container GPU host requirements are not supported by this runtime")
	}
	return nil
}
