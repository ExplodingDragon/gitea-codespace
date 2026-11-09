// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"gitea.dev/codespace/internal/runtimeendpoint"
	"gitea.dev/codespace/internal/webide"
	"golang.org/x/sys/unix"
)

const webIDEStableDuration = 30 * time.Second

type webIDESupervisor struct {
	entrypoint    string
	workdir       string
	lockPath      string
	output        io.Writer
	restartDelay  time.Duration
	maximumDelay  time.Duration
	stableRuntime time.Duration
}

// SuperviseWebIDE keeps the selected provider available without interpreting
// its command line. Provider images own their launch contract through bin/serve.
func SuperviseWebIDE(ctx context.Context) error {
	entrypoint := filepath.Join(webide.ContainerProgramPath, webide.ServeProgram)
	if err := unix.Access(entrypoint, unix.X_OK); err != nil {
		return fmt.Errorf("inspect Web IDE provider serve program: %w", err)
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
		entrypoint: entrypoint, workdir: workdir,
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

// WaitForWebIDE waits for the provider to accept a local connection before
// its endpoint is published through the Agent and Gateway.
func WaitForWebIDE(ctx context.Context) error {
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(runtimeendpoint.WorkspaceEndpointPort))
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("web IDE did not listen on %s within 30 seconds", address)
		case <-ticker.C:
		}
	}
}
