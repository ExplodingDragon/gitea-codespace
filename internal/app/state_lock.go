// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const stateDirLockFileName = "manager.lock"

type stateDirLock struct {
	file *os.File
}

func acquireStateDirLock(stateDir string) (*stateDirLock, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return nil, fmt.Errorf("manager.state_dir is required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, stateDirLockFileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state dir lock %s: %w", path, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("manager state dir %s is already locked", stateDir)
		}
		return nil, fmt.Errorf("lock state dir %s: %w", stateDir, err)
	}
	return &stateDirLock{file: file}, nil
}

func (l *stateDirLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
		_ = l.file.Close()
		l.file = nil
		return fmt.Errorf("unlock state dir: %w", err)
	}
	if err := l.file.Close(); err != nil {
		l.file = nil
		return fmt.Errorf("close state dir lock: %w", err)
	}
	l.file = nil
	return nil
}
