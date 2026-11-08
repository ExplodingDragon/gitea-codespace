// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gitea.dev/codespace/internal/runtimeendpoint"
	"gitea.dev/codespace/internal/webide"
	"golang.org/x/sys/unix"
)

const (
	maxWebIDESettings    = 1024 * 1024
	webIDEStableDuration = 30 * time.Second
	platformExtensionID  = "gitea.codespace"
)

var codeServerArguments = []string{
	"--bind-addr", "0.0.0.0:" + fmt.Sprint(runtimeendpoint.WorkspaceEndpointPort),
	"--auth", "none",
	"--disable-telemetry",
	"--disable-update-check",
	"--user-data-dir", webide.ContainerUserDataPath,
	"--extensions-dir", webide.ContainerExtensionsPath,
}

// PrepareWebIDE writes the initial repository settings before the supervised
// Web IDE starts. Resume and process restarts do not call this operation, so
// subsequent user changes remain intact.
func PrepareWebIDE(settings io.Reader) error {
	return prepareWebIDE(settings, webide.ContainerUserDataPath)
}

func prepareWebIDE(settings io.Reader, userDataPath string) error {
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
	directory := filepath.Join(userDataPath, "User")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Web IDE settings directory: %w", err)
	}
	settingsPath := filepath.Join(directory, "settings.json")
	if _, err := os.Stat(settingsPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Web IDE settings: %w", err)
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
	if err := os.Rename(temporaryPath, settingsPath); err != nil {
		return fmt.Errorf("replace Web IDE settings: %w", err)
	}
	return nil
}

type webIDESupervisor struct {
	entrypoint    string
	arguments     []string
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
	entrypoint := filepath.Join(webide.ContainerProgramPath, "bin", "code-server")
	if err := unix.Access(entrypoint, unix.X_OK); err != nil {
		return fmt.Errorf("inspect platform code-server: %w", err)
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
		entrypoint: entrypoint, arguments: codeServerArguments, workdir: workdir,
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
		arguments := append([]string(nil), s.arguments...)
		arguments = append(arguments, s.workdir)
		command := exec.Command(s.entrypoint, arguments...)
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

// EnsurePlatformExtension installs the required Codespace extension when its
// bundled version is not present in the persistent extension directory.
func EnsurePlatformExtension(ctx context.Context, output io.Writer) error {
	codeServer := filepath.Join(webide.ContainerProgramPath, "bin", "code-server")
	identity, err := platformExtensionIdentity(webide.ContainerVSIXPath)
	if err != nil {
		return err
	}
	arguments := []string{"--user-data-dir", webide.ContainerUserDataPath, "--extensions-dir", webide.ContainerExtensionsPath, "--list-extensions", "--show-versions"}
	command := exec.CommandContext(ctx, codeServer, arguments...)
	command.Stderr = output
	installed, err := command.Output()
	if err != nil {
		return fmt.Errorf("list installed Web IDE extensions: %w", err)
	}
	for _, line := range bytes.Split(installed, []byte{'\n'}) {
		if strings.EqualFold(strings.TrimSpace(string(line)), identity) {
			return nil
		}
	}
	install := exec.CommandContext(ctx, codeServer,
		"--user-data-dir", webide.ContainerUserDataPath,
		"--extensions-dir", webide.ContainerExtensionsPath,
		"--force", "--install-extension", webide.ContainerVSIXPath,
	)
	install.Stdout, install.Stderr = output, output
	if err := install.Run(); err != nil {
		return fmt.Errorf("install required Codespace extension %s: %w", identity, err)
	}
	return nil
}

func platformExtensionIdentity(path string) (string, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("open Codespace extension: %w", err)
	}
	defer archive.Close()
	for _, file := range archive.File {
		if file.Name != "extension/package.json" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return "", err
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, 1<<20))
		closeErr := reader.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return "", err
		}
		var manifest struct {
			Publisher string `json:"publisher"`
			Name      string `json:"name"`
			Version   string `json:"version"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil || manifest.Publisher == "" || manifest.Name == "" || manifest.Version == "" {
			return "", fmt.Errorf("codespace extension manifest is invalid")
		}
		if !strings.EqualFold(manifest.Publisher+"."+manifest.Name, platformExtensionID) {
			return "", fmt.Errorf("codespace extension identity is invalid")
		}
		return manifest.Publisher + "." + manifest.Name + "@" + manifest.Version, nil
	}
	return "", fmt.Errorf("codespace extension manifest is missing")
}
