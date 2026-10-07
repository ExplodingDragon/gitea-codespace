// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"golang.org/x/sys/unix"
)

// ResourceSampler reads the Runtime Pod's cgroup and PVC filesystem. CPU use
// is a rate between samples; memory and disk are point-in-time observations.
type ResourceSampler struct {
	mu        sync.Mutex
	lastCPU   int64
	lastClock time.Time
}

func (s *ResourceSampler) Sample() *codespacev1.RuntimeResourceUsage {
	now := time.Now()
	usedCPU, _ := cgroupValue("/sys/fs/cgroup/cpu.stat", "usage_usec")
	limitCPU := int64(0)
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) == 2 && parts[0] != "max" {
			quota, quotaErr := strconv.ParseInt(parts[0], 10, 64)
			period, periodErr := strconv.ParseInt(parts[1], 10, 64)
			if quotaErr == nil && periodErr == nil && quota >= 0 && period > 0 {
				limitCPU = quota * 1000 / period
			}
		}
	}
	s.mu.Lock()
	usedMillicores := int64(0)
	if s.lastCPU > 0 && usedCPU >= s.lastCPU && now.After(s.lastClock) {
		usedMillicores = (usedCPU - s.lastCPU) * 1000 / now.Sub(s.lastClock).Microseconds()
	}
	s.lastCPU, s.lastClock = usedCPU, now
	s.mu.Unlock()
	memory, _ := fileInt64("/sys/fs/cgroup/memory.current")
	memoryLimit := int64(0)
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil && strings.TrimSpace(string(data)) != "max" {
		memoryLimit, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	}
	var filesystem unix.Statfs_t
	diskUsed, diskLimit := int64(0), int64(0)
	if err := unix.Statfs(runtimeVolume, &filesystem); err == nil {
		diskLimit = int64(filesystem.Blocks) * filesystem.Bsize
		diskUsed = int64(filesystem.Blocks-filesystem.Bavail) * filesystem.Bsize
	}
	return &codespacev1.RuntimeResourceUsage{
		Cpu:          &codespacev1.RuntimeCPUUsage{UsedMillicores: max(usedMillicores, 0), LimitMillicores: max(limitCPU, 0)},
		Memory:       &codespacev1.RuntimeMemoryUsage{UsedBytes: max(memory, 0), LimitBytes: max(memoryLimit, 0)},
		Disk:         &codespacev1.RuntimeDiskUsage{UsedBytes: max(diskUsed, 0), LimitBytes: max(diskLimit, 0)},
		ObservedUnix: now.Unix(),
	}
}

func cgroupValue(path, name string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[0] == name {
			return strconv.ParseInt(parts[1], 10, 64)
		}
	}
	return 0, os.ErrNotExist
}

func fileInt64(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
