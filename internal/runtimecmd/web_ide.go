// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	webIDEEntrypoint     = "/usr/local/bin/code-server-entrypoint"
	maxWebIDESettings    = 1024 * 1024
	webIDEStableDuration = 30 * time.Second
)

// PrepareWebIDE writes the initial repository settings before the supervised
// Web IDE starts. Resume and process restarts do not call this operation, so
// subsequent user changes remain intact.
func PrepareWebIDE(settings io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(settings, maxWebIDESettings+1))
	if err != nil {
		return fmt.Errorf("read Web IDE settings: %w", err)
	}
	if len(data) > maxWebIDESettings {
		return fmt.Errorf("settings for Web IDE exceed %d bytes", maxWebIDESettings)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil || value == nil {
		return fmt.Errorf("settings for Web IDE must be a JSON object")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve Web IDE user home: %w", err)
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	directory := filepath.Join(dataHome, "code-server", "User")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Web IDE settings directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".settings-*.json")
	if err != nil {
		return fmt.Errorf("create Web IDE settings file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	err = temporary.Chmod(0o600)
	if err == nil {
		_, err = temporary.Write(data)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write Web IDE settings: %w", err)
	}
	if err := os.Rename(temporaryPath, filepath.Join(directory, "settings.json")); err != nil {
		return fmt.Errorf("replace Web IDE settings: %w", err)
	}
	return nil
}

type webIDESupervisor struct {
	entrypoint    string
	workdir       string
	lockPath      string
	output        io.Writer
	restartDelay  time.Duration
	maximumDelay  time.Duration
	stableRuntime time.Duration
}

// SuperviseWebIDE keeps the platform Web IDE available for the lifetime of the
// Dev Container. It intentionally owns only this optional service.
func SuperviseWebIDE(ctx context.Context) error {
	if err := unix.Access(webIDEEntrypoint, unix.X_OK); err != nil {
		return fmt.Errorf("inspect code-server Feature entrypoint: %w", err)
	}
	runtimeDirectory := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDirectory == "" {
		cacheDirectory, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("resolve Web IDE runtime directory: %w", err)
		}
		runtimeDirectory = filepath.Join(cacheDirectory, "gitea-codespace")
	} else {
		runtimeDirectory = filepath.Join(runtimeDirectory, "gitea-codespace")
	}
	if err := os.MkdirAll(runtimeDirectory, 0o700); err != nil {
		return fmt.Errorf("create Web IDE runtime directory: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(runtimeDirectory, "web-ide.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open Web IDE supervisor log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	workdir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve Web IDE workspace: %w", err)
	}
	return (webIDESupervisor{
		entrypoint: webIDEEntrypoint, workdir: workdir,
		lockPath: filepath.Join(runtimeDirectory, "web-ide.lock"), output: logFile,
		restartDelay: time.Second, maximumDelay: 10 * time.Second, stableRuntime: webIDEStableDuration,
	}).run(ctx)
}

func (s webIDESupervisor) run(ctx context.Context) error {
	lock, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open Web IDE supervisor lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		return fmt.Errorf("lock Web IDE supervisor: %w", err)
	}
	delay := s.restartDelay
	for ctx.Err() == nil {
		started := time.Now()
		command := exec.Command(s.entrypoint)
		command.Dir = s.workdir
		command.Env = os.Environ()
		command.Stdout, command.Stderr = s.output, s.output
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := command.Start(); err != nil {
			_, _ = fmt.Fprintf(s.output, "Web IDE failed to start: %v\n", err)
		} else {
			_, _ = fmt.Fprintf(s.output, "Web IDE started with process %d\n", command.Process.Pid)
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				_, _ = fmt.Fprintf(s.output, "Web IDE process exited: %v\n", err)
			case <-ctx.Done():
				_ = unix.Kill(-command.Process.Pid, unix.SIGTERM)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
					<-done
				}
				return nil
			}
		}
		if time.Since(started) >= s.stableRuntime {
			delay = s.restartDelay
		}
		_, _ = fmt.Fprintf(s.output, "Web IDE will restart in %s\n", delay)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
		if delay < s.maximumDelay {
			delay *= 2
			if delay > s.maximumDelay {
				delay = s.maximumDelay
			}
		}
	}
	return nil
}
