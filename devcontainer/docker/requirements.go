// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	dockerunits "github.com/docker/go-units"
	"golang.org/x/sys/unix"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/cgroup"
)

func (e *Engine) checkHostRequirements(requirements devcontainer.HostRequirements, workspace string) error {
	var reader cgroup.Reader
	if e.hostCgroup != nil {
		reader = *e.hostCgroup
	} else if requirements.CPUs != nil || strings.TrimSpace(requirements.Memory) != "" {
		var err error
		reader, _, err = cgroup.Discover()
		if err != nil {
			return fmt.Errorf("inspect runtime cgroup: %w", err)
		}
	}
	if requirements.CPUs != nil && *requirements.CPUs > runtime.NumCPU() {
		return fmt.Errorf("dev container requires %d CPUs but the runtime provides %d", *requirements.CPUs, runtime.NumCPU())
	}
	if requirements.CPUs != nil {
		data, err := reader.Read("cpu.max")
		if err != nil {
			return fmt.Errorf("inspect runtime CPU limit: %w", err)
		}
		quota, period, unlimited, err := cgroup.ParseCPUQuota(data)
		if err != nil {
			return fmt.Errorf("inspect runtime CPU limit: %w", err)
		}
		if !unlimited && int64(*requirements.CPUs)*period > quota {
			return fmt.Errorf("dev container requires %d CPUs but the runtime CPU quota is %d/%d", *requirements.CPUs, quota, period)
		}
	}
	if strings.TrimSpace(requirements.Memory) != "" {
		required, err := dockerunits.RAMInBytes(requirements.Memory)
		if err != nil || required <= 0 {
			return fmt.Errorf("dev container hostRequirements.memory %q is invalid", requirements.Memory)
		}
		content, err := reader.Read("memory.max")
		if err != nil {
			return fmt.Errorf("inspect runtime memory: %w", err)
		}
		limit, err := cgroup.ParseMemoryMax(content)
		if err != nil {
			return fmt.Errorf("inspect runtime memory: %w", err)
		}
		var info unix.Sysinfo_t
		if err := unix.Sysinfo(&info); err != nil {
			return fmt.Errorf("inspect runtime memory: %w", err)
		}
		visible := info.Totalram * uint64(info.Unit)
		available := visible
		if limit > 0 && uint64(limit) < available {
			available = uint64(limit)
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
		if info.Bsize <= 0 {
			return fmt.Errorf("inspect runtime storage: invalid filesystem block size")
		}
		available := info.Bavail * uint64(info.Bsize)
		if uint64(required) > available {
			return fmt.Errorf("dev container requires %s storage but the runtime provides %s", requirements.Storage, dockerunits.BytesSize(float64(available)))
		}
	}
	return nil
}

func (e *Engine) resolveGPURequest(ctx context.Context, requirement []byte, platformProvidesGPU bool) (bool, error) {
	value := strings.TrimSpace(string(requirement))
	if value == "" || value == "null" || value == "false" {
		return false, nil
	}
	if platformProvidesGPU {
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
