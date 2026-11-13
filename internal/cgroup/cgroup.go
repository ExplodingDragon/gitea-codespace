// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package cgroup contains cgroup v2 discovery and delegation helpers.
package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/moby/sys/mountinfo"
)

// Reader reads files below one cgroup v2 directory.
type Reader struct {
	root string
}

// At creates a reader for a known cgroup directory. It is also useful for
// deterministic tests.
func At(root string) Reader {
	return Reader{root: path.Clean(root)}
}

// Discover locates the process and the root of its mounted cgroup v2 hierarchy.
// A container's private hierarchy root includes its nested containers even
// after the initial processes have moved into a child cgroup.
func Discover() (current, hierarchy Reader, err error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return Reader{}, Reader{}, fmt.Errorf("read process cgroup: %w", err)
	}
	mounts, err := mountinfo.GetMounts(func(info *mountinfo.Info) (skip, stop bool) {
		return info.FSType != "cgroup2", false
	})
	if err != nil {
		return Reader{}, Reader{}, fmt.Errorf("read cgroup mounts: %w", err)
	}
	return resolveHierarchy(data, mounts)
}

func resolveHierarchy(data []byte, mounts []*mountinfo.Info) (current, hierarchy Reader, err error) {
	processPath := ""
	for line := range strings.Lines(string(data)) {
		parts := strings.SplitN(strings.TrimSuffix(line, "\n"), ":", 3)
		if len(parts) == 3 && parts[0] == "0" {
			processPath = path.Clean(parts[2])
			break
		}
	}
	if processPath == "" || processPath == "." || !path.IsAbs(processPath) {
		return Reader{}, Reader{}, errors.New("process is not in a cgroup v2 hierarchy")
	}
	for _, mount := range mounts {
		root, err := resolveMountPath(processPath, mount.Root, mount.Mountpoint)
		if err == nil {
			return At(root), At(mount.Mountpoint), nil
		}
	}
	return Reader{}, Reader{}, errors.New("process cgroup is not in a mounted cgroup v2 hierarchy")
}

func resolveMountPath(processPath, mountRoot, mountpoint string) (string, error) {
	processPath = path.Clean(processPath)
	mountRoot = path.Clean(mountRoot)
	if !path.IsAbs(processPath) || !path.IsAbs(mountRoot) || !path.IsAbs(mountpoint) {
		return "", errors.New("cgroup mount paths must be absolute")
	}
	if mountRoot != "/" {
		switch {
		case processPath == mountRoot:
			processPath = "/"
		case strings.HasPrefix(processPath, mountRoot+"/"):
			processPath = strings.TrimPrefix(processPath, mountRoot)
		default:
			return "", errors.New("process cgroup is outside mounted hierarchy")
		}
	}
	return path.Join(mountpoint, processPath), nil
}

// Read reads a relative cgroup file.
func (r Reader) Read(name string) ([]byte, error) {
	clean := path.Clean(name)
	if r.root == "" || path.IsAbs(name) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, errors.New("invalid cgroup file name")
	}
	return os.ReadFile(path.Join(r.root, clean))
}

// ReadInt reads a single non-negative integer file.
func (r Reader) ReadInt(name string) (int64, error) {
	data, err := r.Read(name)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("invalid cgroup integer")
	}
	return value, nil
}

// Value reads a named non-negative integer from a cgroup key/value file.
func (r Reader) Value(name, field string) (int64, error) {
	data, err := r.Read(name)
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[0] == field {
			value, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil || value < 0 {
				return 0, errors.New("invalid cgroup value")
			}
			return value, nil
		}
	}
	return 0, os.ErrNotExist
}

// ParseCPUQuota parses cpu.max into quota and period. unlimited means the
// hierarchy explicitly has no CPU quota.
func ParseCPUQuota(data []byte) (quota, period int64, unlimited bool, err error) {
	parts := strings.Fields(string(data))
	if len(parts) != 2 {
		return 0, 0, false, errors.New("invalid cpu.max")
	}
	period, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || period <= 0 {
		return 0, 0, false, errors.New("invalid cpu.max period")
	}
	if parts[0] == "max" {
		return 0, period, true, nil
	}
	quota, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || quota <= 0 {
		return 0, 0, false, errors.New("invalid cpu.max quota")
	}
	return quota, period, false, nil
}

// ParseMemoryMax parses a memory limit. Zero represents an explicit max.
func ParseMemoryMax(data []byte) (int64, error) {
	value := strings.TrimSpace(string(data))
	if value == "max" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit <= 0 {
		return 0, errors.New("invalid memory.max")
	}
	return limit, nil
}
