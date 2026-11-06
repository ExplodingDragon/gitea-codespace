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
	usedCPU, err := cgroupValue("/sys/fs/cgroup/cpu.stat", "usage_usec")
	if err != nil {
		return nil
	}
	cpuMax, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return nil
	}
	limitCPU, err := parseCPUMax(cpuMax)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	usedMillicores := int64(0)
	if s.lastCPU > 0 && usedCPU >= s.lastCPU && now.After(s.lastClock) {
		usedMillicores = (usedCPU - s.lastCPU) * 1000 / now.Sub(s.lastClock).Microseconds()
	}
	s.lastCPU, s.lastClock = usedCPU, now
	s.mu.Unlock()
	memory, err := fileInt64("/sys/fs/cgroup/memory.current")
	if err != nil {
		return nil
	}
	memoryMax, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return nil
	}
	memoryLimit, err := parseMemoryMax(memoryMax)
	if err != nil {
		return nil
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs(runtimeVolume, &filesystem); err != nil {
		return nil
	}
	diskLimit := int64(filesystem.Blocks) * filesystem.Bsize
	if diskLimit <= 0 {
		return nil
	}
	diskUsed := int64(filesystem.Blocks-filesystem.Bavail) * filesystem.Bsize
	return &codespacev1.RuntimeResourceUsage{
		Cpu:          &codespacev1.RuntimeCPUUsage{UsedMillicores: max(usedMillicores, 0), LimitMillicores: limitCPU},
		Memory:       &codespacev1.RuntimeMemoryUsage{UsedBytes: max(memory, 0), LimitBytes: memoryLimit},
		Disk:         &codespacev1.RuntimeDiskUsage{UsedBytes: max(diskUsed, 0), LimitBytes: diskLimit},
		ObservedUnix: now.Unix(),
	}
}

func parseCPUMax(data []byte) (int64, error) {
	parts := strings.Fields(string(data))
	if len(parts) != 2 {
		return 0, os.ErrInvalid
	}
	period, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || period <= 0 {
		return 0, os.ErrInvalid
	}
	if parts[0] == "max" {
		return 0, nil
	}
	quota, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || quota <= 0 {
		return 0, os.ErrInvalid
	}
	return quota * 1000 / period, nil
}

func parseMemoryMax(data []byte) (int64, error) {
	value := strings.TrimSpace(string(data))
	if value == "max" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit <= 0 {
		return 0, os.ErrInvalid
	}
	return limit, nil
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
