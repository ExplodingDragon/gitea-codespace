// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.org/x/sync/errgroup"

	"gitea.dev/codespace/devcontainer"
)

func runInitializeCommand(ctx context.Context, command devcontainer.Command, user devcontainer.HostUser, workdir string, environment map[string]string, stdout, stderr io.Writer) error {
	commands, err := decodeCommand(command)
	if err != nil || len(commands) == 0 {
		return err
	}
	fmt.Fprintln(stdout, "##[group]initializeCommand")
	defer fmt.Fprintln(stdout, "##[endgroup]")
	group, groupCtx := errgroup.WithContext(ctx)
	for _, arguments := range commands {
		group.Go(func() error {
			process := exec.CommandContext(groupCtx, arguments[0], arguments[1:]...)
			process.Dir = workdir
			process.Stdout = stdout
			process.Stderr = stderr
			values := map[string]string{}
			for _, item := range os.Environ() {
				name, value, ok := strings.Cut(item, "=")
				if ok {
					values[name] = value
				}
			}
			for name, value := range environment {
				values[name] = value
			}
			if strings.TrimSpace(user.Name) != "" {
				values["USER"] = user.Name
				values["LOGNAME"] = user.Name
			}
			if strings.TrimSpace(user.Home) != "" {
				values["HOME"] = user.Home
			}
			process.Env = environmentList(values)
			if user.UID != 0 || user.GID != 0 {
				process.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: user.UID, Gid: user.GID}}
			}
			if err := process.Run(); err != nil {
				return fmt.Errorf("command %q: %w", arguments[0], err)
			}
			return nil
		})
	}
	return group.Wait()
}

func decodeCommand(command devcontainer.Command) ([][]string, error) {
	if len(command.Value) == 0 || bytes.Equal(bytes.TrimSpace(command.Value), []byte("null")) {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(command.Value, &value); err != nil {
		return nil, err
	}
	toArguments := func(value any) ([]string, error) {
		switch value := value.(type) {
		case string:
			return []string{"/bin/sh", "-c", value}, nil
		case []any:
			arguments := make([]string, len(value))
			for i, item := range value {
				arguments[i], _ = item.(string)
			}
			if len(arguments) == 0 || strings.TrimSpace(arguments[0]) == "" {
				return nil, fmt.Errorf("lifecycle command is empty")
			}
			return arguments, nil
		default:
			return nil, fmt.Errorf("lifecycle command has an invalid value")
		}
	}
	if object, ok := value.(map[string]any); ok {
		names := make([]string, 0, len(object))
		for name := range object {
			names = append(names, name)
		}
		sort.Strings(names)
		result := make([][]string, 0, len(names))
		for _, name := range names {
			arguments, err := toArguments(object[name])
			if err != nil {
				return nil, fmt.Errorf("lifecycle command %s: %w", name, err)
			}
			result = append(result, arguments)
		}
		return result, nil
	}
	arguments, err := toArguments(value)
	if err != nil {
		return nil, err
	}
	return [][]string{arguments}, nil
}

func (e *Engine) runLifecycleCommand(ctx context.Context, environment *devcontainer.State, name string, command devcontainer.Command, secrets map[string]string) error {
	commands, err := decodeCommand(command)
	if err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	if len(commands) == 0 {
		return nil
	}
	fmt.Fprintf(e.stdout, "##[group]%s\n", name)
	defer fmt.Fprintln(e.stdout, "##[endgroup]")
	group, groupCtx := errgroup.WithContext(ctx)
	for _, arguments := range commands {
		group.Go(func() error {
			environmentValues := devcontainer.ProcessEnvironment(environment.RemoteEnvironment, secrets)
			if _, _, err := e.Exec(groupCtx, environment.PrimaryContainerID, environment.RemoteUser, environment.RemoteWorkdir, arguments, environmentValues, nil); err != nil {
				return fmt.Errorf("run %s: %w", name, err)
			}
			return nil
		})
	}
	return group.Wait()
}

// RunPostAttach runs postAttachCommand for a new user attachment.
func (e *Engine) RunPostAttach(ctx context.Context, environment *devcontainer.State, secrets map[string]string) error {
	return e.runLifecycleCommand(ctx, environment, "postAttachCommand", environment.Configuration.PostAttachCommand, secrets)
}

// Exec runs a non-interactive command in one environment container.
func (e *Engine) Exec(ctx context.Context, containerID, user, workdir string, command []string, environment map[string]string, stdin io.Reader) ([]byte, []byte, error) {
	created, err := e.client.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User:         user,
		WorkingDir:   workdir,
		Cmd:          command,
		Env:          environmentList(environment),
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create container exec: %w", err)
	}
	attached, err := e.client.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("attach container exec: %w", err)
	}
	defer attached.Close()
	if stdin != nil {
		go func() {
			_, _ = io.Copy(attached.Conn, stdin)
			_ = attached.CloseWrite()
		}()
	}
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(io.MultiWriter(e.stdout, &stdout), io.MultiWriter(e.stderr, &stderr), attached.Reader); err != nil {
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("read container exec output: %w", err)
	}
	result, err := e.client.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("inspect container exec: %w", err)
	}
	if result.ExitCode != 0 {
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("container command exited with status %d", result.ExitCode)
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

// CopyFile copies one host file into a container with the requested mode.
func (e *Engine) CopyFile(ctx context.Context, containerID, source, target string, mode int64) error {
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open source file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat runtime binary: %w", err)
	}
	return e.copyContent(ctx, containerID, target, mode, info.Size(), file)
}

// CopyContent copies in-memory content into a container with the requested mode.
func (e *Engine) CopyContent(ctx context.Context, containerID, target string, mode int64, content []byte) error {
	return e.copyContent(ctx, containerID, target, mode, int64(len(content)), bytes.NewReader(content))
}

func (e *Engine) copyContent(ctx context.Context, containerID, target string, mode, size int64, content io.Reader) error {
	reader, writer := io.Pipe()
	go func() {
		archive := tar.NewWriter(writer)
		err := archive.WriteHeader(&tar.Header{Name: filepath.Base(target), Mode: mode, Size: size})
		if err == nil {
			_, err = io.Copy(archive, content)
		}
		err = errors.Join(err, archive.Close())
		_ = writer.CloseWithError(err)
	}()
	if err := e.client.CopyToContainer(ctx, containerID, filepath.Dir(target), reader, container.CopyToContainerOptions{}); err != nil {
		_ = reader.CloseWithError(err)
		return fmt.Errorf("install runtime helper in Dev Container: %w", err)
	}
	if err := reader.Close(); err != nil {
		return fmt.Errorf("stream runtime helper: %w", err)
	}
	return nil
}

func (e *Engine) probeRemoteEnvironment(ctx context.Context, environment *devcontainer.State) (map[string]string, error) {
	if environment.Configuration.UserEnvProbe == "none" {
		return map[string]string{}, nil
	}
	flag := "-lc"
	switch environment.Configuration.UserEnvProbe {
	case "loginInteractiveShell":
		flag = "-lic"
	case "interactiveShell":
		flag = "-ic"
	}
	shellOutput, _, _ := e.Exec(ctx, environment.PrimaryContainerID, environment.RemoteUser, environment.RemoteWorkdir, []string{"/bin/sh", "-c", `user="${1%%:*}"; getent passwd "$user" 2>/dev/null | cut -d: -f7`, "sh", environment.RemoteUser}, nil, nil)
	shell := strings.TrimSpace(string(shellOutput))
	if shell == "" {
		shell = "/bin/sh"
	}
	stdout, _, err := e.Exec(ctx, environment.PrimaryContainerID, environment.RemoteUser, environment.RemoteWorkdir, []string{shell, flag, "env -0"}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("probe Dev Container user environment: %w", err)
	}
	result := map[string]string{}
	for _, item := range bytes.Split(stdout, []byte{0}) {
		name, value, ok := bytes.Cut(item, []byte{'='})
		if ok && len(name) > 0 {
			result[string(name)] = string(value)
		}
	}
	return result, nil
}
