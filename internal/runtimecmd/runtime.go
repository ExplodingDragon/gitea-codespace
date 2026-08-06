// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/moby/term"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"gitea.dev/codespace/internal/devcontainerruntime"
)

const containerRuntimeBinary = devcontainerruntime.ContainerRuntimeBinary

// ExecOptions describes a command executed in the primary Dev Container.
type ExecOptions struct {
	StatePath   string
	SecretsPath string
	Interactive bool
	Command     []string
}

// Check verifies that the persisted Dev Container runtime is available.
func Check(ctx context.Context, statePath string, stdout, stderr io.Writer) error {
	state, err := readState(statePath)
	if err != nil {
		return err
	}
	engine, err := containerdocker.New(ctx, stdout, stderr)
	if err != nil {
		return err
	}
	defer func() { _ = engine.Close() }()
	_, err = engine.Inspect(ctx, state)
	return err
}

// Apply applies a lifecycle request and writes its structured result.
func Apply(ctx context.Context, requestPath, resultPath string, stdout, stderr io.Writer) error {
	if !filepath.IsAbs(requestPath) || !filepath.IsAbs(resultPath) {
		return fmt.Errorf("runtime request and result paths must be absolute")
	}
	var request devcontainerruntime.Request
	if err := readJSON(requestPath, &request); err != nil {
		return writeJSONAtomic(resultPath, devcontainerruntime.Result{
			Version: devcontainerruntime.FormatVersion,
			Error:   fmt.Sprintf("decode runtime request: %v", err),
		})
	}
	if err := request.Validate(); err != nil {
		return writeJSONAtomic(resultPath, devcontainerruntime.Result{
			Version: devcontainerruntime.FormatVersion,
			Error:   err.Error(),
		})
	}
	engine, err := containerdocker.New(ctx, stdout, stderr)
	if err != nil {
		return writeJSONAtomic(resultPath, devcontainerruntime.Result{
			Version:     devcontainerruntime.FormatVersion,
			Error:       err.Error(),
			Recoverable: true,
		})
	}
	defer func() { _ = engine.Close() }()
	var state *devcontainer.State
	switch request.Action {
	case "create":
		options, optionsErr := devcontainerruntime.BuildCreateOptions(request)
		if optionsErr != nil {
			err = devcontainer.InvalidConfiguration(optionsErr)
			break
		}
		options.PrepareLifecycle = func(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
			return devcontainerruntime.ConfigureCreate(ctx, engine, state, request)
		}
		state, err = engine.Create(ctx, options)
		if err == nil {
			err = devcontainerruntime.StartWorkspaceServices(ctx, engine, state, request.Secrets, devcontainerruntime.WorkspaceServiceOptions{InitializeWebIDE: true}, stdout, stderr)
			if err != nil {
				_ = engine.Delete(context.WithoutCancel(ctx), state)
			}
		}
	case "resume":
		state, err = engine.Start(ctx, request.Environment, request.Secrets)
		if err == nil {
			err = devcontainerruntime.StartWorkspaceServices(ctx, engine, state, request.Secrets, devcontainerruntime.WorkspaceServiceOptions{}, stdout, stderr)
		}
	case "stop":
		state, err = engine.Stop(ctx, request.Environment)
	case "inspect":
		state, err = engine.Inspect(ctx, request.Environment)
	}
	if err != nil {
		return writeJSONAtomic(resultPath, devcontainerruntime.Result{
			Version:     devcontainerruntime.FormatVersion,
			Error:       err.Error(),
			Recoverable: !devcontainerruntime.IsFormatError(err) && !devcontainer.IsInvalidConfiguration(err),
		})
	}
	return writeJSONAtomic(resultPath, devcontainerruntime.Result{Version: devcontainerruntime.FormatVersion, Environment: state})
}

// Exec runs a command in the primary Dev Container.
func Exec(ctx context.Context, options ExecOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	command := options.Command
	if len(command) == 0 {
		command = []string{"/bin/sh", "-l"}
	}
	state, err := readState(options.StatePath)
	if err != nil {
		return err
	}
	secrets := map[string]string{}
	if strings.TrimSpace(options.SecretsPath) != "" {
		secrets, err = readStringMap(options.SecretsPath)
		if err != nil {
			return fmt.Errorf("read runtime secrets: %w", err)
		}
	}
	values := devcontainer.ProcessEnvironment(state.RemoteEnvironment, secrets)
	if options.Interactive {
		values["TERM"] = "xterm-256color"
		values["COLORTERM"] = "truecolor"
	}
	attachEngine, err := containerdocker.New(ctx, io.Discard, stderr)
	if err != nil {
		return err
	}
	if err := attachEngine.RunPostAttach(ctx, state, secrets); err != nil {
		_ = attachEngine.Close()
		return err
	}
	if err := attachEngine.Close(); err != nil {
		return err
	}
	apiClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("create Docker client: %w", err)
	}
	defer func() { _ = apiClient.Close() }()
	created, err := apiClient.ContainerExecCreate(ctx, state.PrimaryContainerID, container.ExecOptions{
		User:         state.RemoteUser,
		WorkingDir:   state.RemoteWorkdir,
		Env:          stringMapEnvironment(values),
		Cmd:          command,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          options.Interactive,
	})
	if err != nil {
		return fmt.Errorf("create workspace exec: %w", err)
	}
	attached, err := apiClient.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{Tty: options.Interactive})
	if err != nil {
		return fmt.Errorf("attach workspace exec: %w", err)
	}
	defer func() { attached.Close() }()
	go func() {
		_, _ = io.Copy(attached.Conn, stdin)
		_ = attached.CloseWrite()
	}()
	if options.Interactive {
		resize := func() {
			if file, ok := stdin.(*os.File); ok {
				if size, err := term.GetWinsize(file.Fd()); err == nil {
					_ = apiClient.ContainerExecResize(ctx, created.ID, container.ResizeOptions{Height: uint(size.Height), Width: uint(size.Width)})
				}
			}
		}
		resize()
		signals := make(chan os.Signal, 1)
		resizeDone := make(chan struct{})
		signal.Notify(signals, syscall.SIGWINCH)
		defer func() {
			signal.Stop(signals)
			close(resizeDone)
		}()
		go func() {
			for {
				select {
				case <-resizeDone:
					return
				case <-signals:
					resize()
				}
			}
		}()
		_, err = io.Copy(stdout, attached.Reader)
	} else {
		_, err = stdcopy.StdCopy(stdout, stderr, attached.Reader)
	}
	if err != nil {
		return fmt.Errorf("read workspace exec: %w", err)
	}
	result, err := apiClient.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return fmt.Errorf("inspect workspace exec: %w", err)
	}
	if result.ExitCode != 0 {
		return &ExitError{Status: result.ExitCode}
	}
	return nil
}

// TCP bridges a stream to a loopback port in the primary Dev Container.
func TCP(ctx context.Context, statePath string, port uint16, stdin io.Reader, stdout io.Writer) error {
	if port == 0 {
		return fmt.Errorf("runtime TCP port is invalid")
	}
	state, err := readState(statePath)
	if err != nil {
		return err
	}
	apiClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer func() { _ = apiClient.Close() }()
	created, err := apiClient.ContainerExecCreate(ctx, state.PrimaryContainerID, container.ExecOptions{
		Cmd:          []string{containerRuntimeBinary, "runtime", "connect", "--host", "localhost", "--port", strconv.Itoa(int(port))},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
	})
	if err != nil {
		return fmt.Errorf("create runtime TCP bridge: %w", err)
	}
	attached, err := apiClient.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{Tty: false})
	if err != nil {
		return fmt.Errorf("attach runtime TCP bridge: %w", err)
	}
	defer func() { attached.Close() }()
	go func() {
		_, _ = io.Copy(attached.Conn, stdin)
		_ = attached.CloseWrite()
	}()
	if _, err := stdcopy.StdCopy(stdout, io.Discard, attached.Reader); err != nil {
		return fmt.Errorf("read runtime TCP bridge: %w", err)
	}
	result, err := apiClient.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return &ExitError{Status: result.ExitCode}
	}
	return nil
}

// Connect bridges a stream to an allowed loopback address.
func Connect(ctx context.Context, host string, port uint16, stdin io.Reader, stdout io.Writer) error {
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("runtime TCP host must be localhost")
	}
	if port == 0 {
		return fmt.Errorf("runtime TCP port is invalid")
	}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return fmt.Errorf("connect Dev Container localhost port: %w", err)
	}
	defer func() { _ = connection.Close() }()
	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(connection, stdin)
		if stream, ok := connection.(*net.TCPConn); ok {
			_ = stream.CloseWrite()
		}
		copyDone <- err
	}()
	_, outputErr := io.Copy(stdout, connection)
	inputErr := <-copyDone
	return errors.Join(inputErr, outputErr)
}

// ExitError reports the exit status returned by a Dev Container command.
type ExitError struct {
	Status int
}

func (e *ExitError) Error() string {
	return "runtime command exited with status " + strconv.Itoa(e.Status)
}

func readState(path string) (*devcontainer.State, error) {
	var state devcontainer.State
	if err := readJSON(path, &state); err != nil {
		return nil, err
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	return &state, nil
}

func readStringMap(path string) (map[string]string, error) {
	result := map[string]string{}
	return result, readJSON(path, &result)
}

func readJSON(path string, target any) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("runtime file path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(target)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			decodeErr = fmt.Errorf("runtime JSON contains trailing data")
		}
	}
	return errors.Join(decodeErr, file.Close())
}

func writeJSONAtomic(path string, value any) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".runtime-result-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func stringMapEnvironment(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}
