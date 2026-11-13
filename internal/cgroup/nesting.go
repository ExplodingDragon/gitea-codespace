// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// EnableNesting delegates available controllers below a private container
// hierarchy. Its root must be free of processes to create domain children.
func (r Reader) EnableNesting(ctx context.Context) error {
	data, err := r.Read("cgroup.type")
	if err != nil {
		return fmt.Errorf("read cgroup type: %w", err)
	}
	if kind := strings.TrimSpace(string(data)); kind != "domain" {
		return fmt.Errorf("cannot prepare nesting in cgroup type %q; recreate the Runtime Pod", kind)
	}
	data, err = r.Read("cgroup.controllers")
	if err != nil {
		return fmt.Errorf("read delegated controllers: %w", err)
	}
	controllers := strings.Fields(string(data))
	if len(controllers) == 0 {
		return errors.New("no cgroup controllers are delegated to the Runtime")
	}
	for i := range controllers {
		controllers[i] = "+" + controllers[i]
	}
	initGroup := filepath.Join(r.root, "init")
	if err := os.MkdirAll(initGroup, 0o755); err != nil {
		return fmt.Errorf("create initial process cgroup: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := r.Read("cgroup.procs")
		if err != nil {
			return fmt.Errorf("read root cgroup processes: %w", err)
		}
		for _, value := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				return fmt.Errorf("invalid process ID %q in Runtime cgroup", value)
			}
			// Each write moves one process and all its threads. Processes can
			// exit between listing and moving them, which needs no recovery.
			if err := os.WriteFile(filepath.Join(initGroup, "cgroup.procs"), []byte(value), 0o644); err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("move process %d to initial cgroup: %w", pid, err)
			}
		}
		err = os.WriteFile(filepath.Join(r.root, "cgroup.subtree_control"), []byte(strings.Join(controllers, " ")), 0o644)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("enable nested cgroup controllers: %w", err)
		}
		// Concurrent exec can briefly place another process in the root.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
