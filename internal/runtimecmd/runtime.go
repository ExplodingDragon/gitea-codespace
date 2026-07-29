// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
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

	"gitea.dev/codespace/internal/devcontainer"
	containerdocker "gitea.dev/codespace/internal/devcontainer/docker"
)

const containerRuntimeBinary = "/usr/local/libexec/gitea-codespace-runtime"

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("runtime action is required")
	}
	switch args[0] {
	case "apply":
		return runApply(ctx, args[1:], stdout, stderr)
	case "exec":
		return runExec(ctx, args[1:], stdin, stdout, stderr)
	case "tcp":
		return runTCP(ctx, args[1:], stdin, stdout)
	case "check":
		return runCheck(ctx, args[1:], stdout, stderr)
	case "connect":
		return runConnect(ctx, args[1:], stdin, stdout)
	case "endpoint":
		return runEndpoint(args[1:])
	default:
		return fmt.Errorf("runtime action %q is invalid", args[0])
	}
}

func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("runtime check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	statePath := flags.String("state", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	environment, err := readEnvironment(*statePath)
	if err != nil {
		return err
	}
	engine, err := containerdocker.New(ctx, stdout, stderr)
	if err != nil {
		return err
	}
	defer engine.Close()
	_, err = engine.Apply(ctx, devcontainer.RuntimeRequest{
		Version:       devcontainer.RuntimeFormatVersion,
		Action:        "inspect",
		CodespaceUUID: environment.CodespaceUUID,
		Environment:   environment,
	})
	return err
}

func runApply(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("runtime apply", flag.ContinueOnError)
	flags.SetOutput(stderr)
	requestPath := flags.String("request", "", "")
	resultPath := flags.String("result", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*requestPath) || !filepath.IsAbs(*resultPath) {
		return fmt.Errorf("runtime request and result paths must be absolute")
	}
	requestFile, err := os.Open(*requestPath)
	if err != nil {
		return fmt.Errorf("open runtime request: %w", err)
	}
	decoder := json.NewDecoder(requestFile)
	decoder.DisallowUnknownFields()
	var request devcontainer.RuntimeRequest
	decodeErr := decoder.Decode(&request)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			decodeErr = fmt.Errorf("runtime request contains trailing data")
		}
	}
	closeErr := requestFile.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return writeJSONAtomic(*resultPath, devcontainer.RuntimeResult{
			Version: devcontainer.RuntimeFormatVersion,
			Error:   fmt.Sprintf("decode runtime request: %v", err),
		})
	}
	if err := request.Validate(); err != nil {
		return writeJSONAtomic(*resultPath, devcontainer.RuntimeResult{
			Version: devcontainer.RuntimeFormatVersion,
			Error:   err.Error(),
		})
	}
	engine, err := containerdocker.New(ctx, stdout, stderr)
	if err != nil {
		return writeJSONAtomic(*resultPath, devcontainer.RuntimeResult{
			Version:     devcontainer.RuntimeFormatVersion,
			Error:       err.Error(),
			Recoverable: true,
		})
	}
	defer engine.Close()
	environment, err := engine.Apply(ctx, request)
	if err != nil {
		return writeJSONAtomic(*resultPath, devcontainer.RuntimeResult{
			Version:     devcontainer.RuntimeFormatVersion,
			Error:       err.Error(),
			Recoverable: !devcontainer.IsFormatError(err) && !devcontainer.IsInvalidConfiguration(err),
		})
	}
	return writeJSONAtomic(*resultPath, devcontainer.RuntimeResult{Version: devcontainer.RuntimeFormatVersion, Environment: environment})
}

func runExec(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("runtime exec", flag.ContinueOnError)
	flags.SetOutput(stderr)
	statePath := flags.String("state", "", "")
	secretsPath := flags.String("secrets", "", "")
	interactive := flags.Bool("interactive", false, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	command := flags.Args()
	if len(command) == 0 {
		command = []string{"/bin/sh", "-l"}
	}
	environment, err := readEnvironment(*statePath)
	if err != nil {
		return err
	}
	values := make(map[string]string, len(environment.RemoteEnvironment))
	for name, value := range environment.RemoteEnvironment {
		values[name] = value
	}
	secrets := map[string]string{}
	if strings.TrimSpace(*secretsPath) != "" {
		secrets, err = readStringMap(*secretsPath)
		if err != nil {
			return fmt.Errorf("read runtime secrets: %w", err)
		}
		for name, value := range secrets {
			values[name] = value
		}
	}
	if *interactive {
		values["TERM"] = "xterm-256color"
		values["COLORTERM"] = "truecolor"
	}
	attachEngine, err := containerdocker.New(ctx, stdout, stderr)
	if err != nil {
		return err
	}
	if err := attachEngine.RunPostAttach(ctx, environment, secrets); err != nil {
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
	defer apiClient.Close()
	created, err := apiClient.ContainerExecCreate(ctx, environment.PrimaryContainerID, container.ExecOptions{
		User:         environment.RemoteUser,
		WorkingDir:   environment.RemoteWorkdir,
		Env:          stringMapEnvironment(values),
		Cmd:          command,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          *interactive,
	})
	if err != nil {
		return fmt.Errorf("create workspace exec: %w", err)
	}
	attached, err := apiClient.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{Tty: *interactive})
	if err != nil {
		return fmt.Errorf("attach workspace exec: %w", err)
	}
	defer attached.Close()
	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(attached.Conn, stdin)
		_ = attached.CloseWrite()
		copyDone <- err
	}()
	if *interactive {
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

func runTCP(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("runtime tcp", flag.ContinueOnError)
	statePath := flags.String("state", "", "")
	port := flags.Uint("port", 0, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *port == 0 || *port > 65535 {
		return fmt.Errorf("runtime TCP port is invalid")
	}
	environment, err := readEnvironment(*statePath)
	if err != nil {
		return err
	}
	apiClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer apiClient.Close()
	created, err := apiClient.ContainerExecCreate(ctx, environment.PrimaryContainerID, container.ExecOptions{
		Cmd:          []string{containerRuntimeBinary, "runtime", "connect", "--host", "localhost", "--port", strconv.Itoa(int(*port))},
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
	defer attached.Close()
	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(attached.Conn, stdin)
		_ = attached.CloseWrite()
		copyDone <- err
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

func runConnect(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("runtime connect", flag.ContinueOnError)
	host := flags.String("host", "localhost", "")
	port := flags.Uint("port", 0, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *host != "localhost" && *host != "127.0.0.1" && *host != "::1" {
		return fmt.Errorf("runtime TCP host must be localhost")
	}
	if *port == 0 || *port > 65535 {
		return fmt.Errorf("runtime TCP port is invalid")
	}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(*host, strconv.Itoa(int(*port))))
	if err != nil {
		return fmt.Errorf("connect Dev Container localhost port: %w", err)
	}
	defer connection.Close()
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

type ExitError struct {
	Status int
}

func (e *ExitError) Error() string {
	return "runtime command exited with status " + strconv.Itoa(e.Status)
}

func readEnvironment(path string) (*devcontainer.Environment, error) {
	var environment devcontainer.Environment
	if err := readJSON(path, &environment); err != nil {
		return nil, err
	}
	if err := environment.Validate(); err != nil {
		return nil, err
	}
	return &environment, nil
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
	defer os.Remove(temporary)
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
