// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/cgroup"
	"golang.org/x/sys/unix"
)

// ResourceSampler reads the Runtime Pod's cgroup and PVC filesystem. CPU use
// is a rate between samples; memory and disk are point-in-time observations.
// Each metric is sampled independently so the Manager can combine sources.
type ResourceSampler struct {
	mu             sync.Mutex
	cgroup         cgroup.Reader
	lastCPU        int64
	lastClock      time.Time
	metricFailures map[string]bool
	volume         string
}

// NewResourceSampler keeps the Runtime hierarchy as its sampling boundary,
// independent of where the Agent and Docker processes live below it.
func NewResourceSampler(reader cgroup.Reader) *ResourceSampler {
	return &ResourceSampler{cgroup: reader, metricFailures: make(map[string]bool), volume: runtimeVolume}
}

func (s *ResourceSampler) recordMetric(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.metricFailures == nil {
		s.metricFailures = make(map[string]bool)
	}
	if err != nil {
		if !s.metricFailures[name] {
			slog.Warn("Read Agent resource metric", "metric", name, "error", err)
		}
		s.metricFailures[name] = true
		return
	}
	if s.metricFailures[name] {
		slog.Info("Agent resource metric recovered", "metric", name)
	}
	delete(s.metricFailures, name)
}

func (s *ResourceSampler) Sample() *codespacev1.RuntimeResourceUsage {
	now := time.Now()
	usage := &codespacev1.RuntimeResourceUsage{}
	var err error
	usage.Cpu, err = s.sampleCPU(now)
	s.recordMetric("cpu", err)
	usage.Memory, err = s.sampleMemory()
	s.recordMetric("memory", err)
	usage.Disk, err = s.sampleDisk()
	s.recordMetric("disk", err)
	if usage.Cpu == nil && usage.Memory == nil && usage.Disk == nil {
		return nil
	}
	usage.ObservedUnix = now.Unix()
	return usage
}

func (s *ResourceSampler) sampleCPU(now time.Time) (*codespacev1.RuntimeCPUUsage, error) {
	used, err := s.cgroup.Value("cpu.stat", "usage_usec")
	if err != nil {
		return nil, err
	}
	limitData, err := s.cgroup.Read("cpu.max")
	if err != nil {
		return nil, err
	}
	quota, period, unlimited, err := cgroup.ParseCPUQuota(limitData)
	if err != nil {
		return nil, err
	}
	limitCPU := int64(0)
	if !unlimited {
		if quota > math.MaxInt64/1000 {
			return nil, errors.New("cpu.max quota is too large")
		}
		limitCPU = quota * 1000 / period
	}
	s.mu.Lock()
	previous, previousAt := s.lastCPU, s.lastClock
	s.lastCPU, s.lastClock = used, now
	s.mu.Unlock()
	if previousAt.IsZero() || !now.After(previousAt) || used < previous {
		return nil, nil
	}
	delta := now.Sub(previousAt).Microseconds()
	if delta <= 0 {
		return nil, nil
	}
	usedMillicores := (used - previous) * 1000 / delta
	if usedMillicores < 0 {
		usedMillicores = 0
	}
	return &codespacev1.RuntimeCPUUsage{UsedMillicores: usedMillicores, LimitMillicores: limitCPU}, nil
}

func (s *ResourceSampler) sampleMemory() (*codespacev1.RuntimeMemoryUsage, error) {
	current, err := s.cgroup.ReadInt("memory.current")
	if err != nil {
		return nil, err
	}
	inactive, err := s.cgroup.Value("memory.stat", "inactive_file")
	if err != nil {
		return nil, err
	}
	limitData, err := s.cgroup.Read("memory.max")
	if err != nil {
		return nil, err
	}
	limitMemory, err := cgroup.ParseMemoryMax(limitData)
	if err != nil {
		return nil, err
	}
	return &codespacev1.RuntimeMemoryUsage{UsedBytes: max(current-inactive, 0), LimitBytes: limitMemory}, nil
}

func (s *ResourceSampler) sampleDisk() (*codespacev1.RuntimeDiskUsage, error) {
	var filesystem unix.Statfs_t
	if err := unix.Statfs(s.volume, &filesystem); err != nil {
		return nil, err
	}
	if filesystem.Bsize <= 0 {
		return nil, errors.New("filesystem block size is invalid")
	}
	blockSize := uint64(filesystem.Bsize)
	if filesystem.Blocks == 0 || filesystem.Blocks > math.MaxInt64/blockSize || filesystem.Bavail > filesystem.Blocks {
		return nil, errors.New("filesystem capacity is invalid")
	}
	capacity := filesystem.Blocks * blockSize
	used := (filesystem.Blocks - filesystem.Bavail) * blockSize
	return &codespacev1.RuntimeDiskUsage{UsedBytes: int64(used), LimitBytes: int64(capacity)}, nil
}
