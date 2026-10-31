// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// MountDataVolume prepares a raw Kubernetes block volume inside an isolated
// runtime. Manager durably authorizes the first format before any device write.
func MountDataVolume(ctx context.Context, device, directory string, prepare func(context.Context, bool) error) (func() error, error) {
	if prepare == nil {
		return nil, fmt.Errorf("volume preparation authorization is required")
	}
	info, err := os.Stat(device)
	if err != nil {
		return nil, fmt.Errorf("inspect data device: %w", err)
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return nil, fmt.Errorf("data device %q is not a block device", device)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create data mount point: %w", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect data mount point: %w", err)
	}
	if len(entries) != 0 {
		return nil, fmt.Errorf("data mount point %q is not empty", directory)
	}

	probe := exec.CommandContext(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", device)
	output, err := probe.Output()
	filesystem := strings.TrimSpace(string(output))
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 2 {
			return nil, fmt.Errorf("inspect data filesystem: %w", err)
		}
		if err := prepare(ctx, false); err != nil {
			return nil, fmt.Errorf("authorize first data volume format: %w", err)
		}
		format := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-m", "0", device)
		if output, err := format.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("format blank data volume: %w: %s", err, strings.TrimSpace(string(output)))
		}
	} else if filesystem != "ext4" {
		return nil, fmt.Errorf("data volume contains unsupported %q filesystem", filesystem)
	}
	check := exec.CommandContext(ctx, "e2fsck", "-p", device)
	if output, err := check.CombinedOutput(); err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			return nil, fmt.Errorf("check data filesystem: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	if err := unix.Mount(device, directory, "ext4", unix.MS_NOATIME, ""); err != nil {
		return nil, fmt.Errorf("mount data volume: %w", err)
	}
	if err := prepare(ctx, true); err != nil {
		return nil, errors.Join(fmt.Errorf("confirm initialized data volume: %w", err), unix.Unmount(directory, 0))
	}
	return func() error {
		if err := unix.Unmount(directory, 0); err != nil {
			return fmt.Errorf("unmount data volume: %w", err)
		}
		return nil
	}, nil
}
