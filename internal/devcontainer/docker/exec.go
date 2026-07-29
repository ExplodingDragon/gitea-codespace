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
	"strconv"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.org/x/sync/errgroup"

	"gitea.dev/codespace/internal/devcontainer"
)

func runInitializeCommand(ctx context.Context, command devcontainer.Command, user devcontainer.RuntimeUser, workdir string, environment map[string]string, stdout, stderr io.Writer) error {
	commands, err := decodeCommand(command)
	if err != nil || len(commands) == 0 {
		return err
	}
	group, groupCtx := errgroup.WithContext(ctx)
	for _, arguments := range commands {
		arguments := arguments
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

func (e *Engine) runLifecycleCommand(ctx context.Context, environment *devcontainer.Environment, name string, command devcontainer.Command, secrets map[string]string) error {
	commands, err := decodeCommand(command)
	if err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	if len(commands) == 0 {
		return nil
	}
	group, groupCtx := errgroup.WithContext(ctx)
	for _, arguments := range commands {
		arguments := arguments
		group.Go(func() error {
			environmentValues := make(map[string]string, len(environment.RemoteEnvironment)+len(secrets))
			for key, value := range environment.RemoteEnvironment {
				environmentValues[key] = value
			}
			for key, value := range secrets {
				environmentValues[key] = value
			}
			if _, _, err := e.exec(groupCtx, environment.PrimaryContainerID, environment.RemoteUser, environment.RemoteWorkdir, arguments, environmentValues, nil); err != nil {
				return fmt.Errorf("run %s: %w", name, err)
			}
			return nil
		})
	}
	return group.Wait()
}

// RunPostAttach applies the Dev Container attachment hook before one interactive tool session.
func (e *Engine) RunPostAttach(ctx context.Context, environment *devcontainer.Environment, secrets map[string]string) error {
	return e.runLifecycleCommand(ctx, environment, "postAttachCommand", environment.Configuration.PostAttachCommand, secrets)
}

func (e *Engine) exec(ctx context.Context, containerID, user, workdir string, command []string, environment map[string]string, stdin io.Reader) ([]byte, []byte, error) {
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

func (e *Engine) copyRuntimeBinary(ctx context.Context, containerID string) error {
	path, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate runtime binary: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open runtime binary: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat runtime binary: %w", err)
	}
	reader, writer := io.Pipe()
	go func() {
		archive := tar.NewWriter(writer)
		err := archive.WriteHeader(&tar.Header{Name: filepath.Base(runtimeBinaryPath), Mode: 0o755, Size: info.Size()})
		if err == nil {
			_, err = io.Copy(archive, file)
		}
		err = errors.Join(err, archive.Close())
		_ = writer.CloseWithError(err)
	}()
	if err := e.client.CopyToContainer(ctx, containerID, filepath.Dir(runtimeBinaryPath), reader, container.CopyToContainerOptions{}); err != nil {
		_ = reader.CloseWithError(err)
		return fmt.Errorf("install runtime helper in Dev Container: %w", err)
	}
	if err := reader.Close(); err != nil {
		return fmt.Errorf("stream runtime helper: %w", err)
	}
	return nil
}

func (e *Engine) probeRemoteEnvironment(ctx context.Context, environment *devcontainer.Environment) (map[string]string, error) {
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
	stdout, _, err := e.exec(ctx, environment.PrimaryContainerID, environment.RemoteUser, environment.RemoteWorkdir, []string{"/bin/sh", flag, "env -0"}, nil, nil)
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

func (e *Engine) updateRemoteUserIdentity(ctx context.Context, environment *devcontainer.Environment, runtimeUser devcontainer.RuntimeUser) error {
	if runtimeUser.UID == 0 || runtimeUser.GID == 0 || environment.RemoteUser == "root" || environment.RemoteUser == "0" {
		return nil
	}
	command := `set -eu
user="$(id -un "$GITEA_REMOTE_USER")"
old_uid="$(id -u "$user")"
old_gid="$(id -g "$user")"
group="$(id -gn "$user")"
if [ "$old_gid" != "$GITEA_RUNTIME_GID" ]; then
  owner="$(getent group "$GITEA_RUNTIME_GID" | cut -d: -f1 || true)"
  [ -z "$owner" ] || [ "$owner" = "$group" ] || { echo "runtime gid is already used by group $owner" >&2; exit 1; }
  groupmod -g "$GITEA_RUNTIME_GID" "$group"
fi
if [ "$old_uid" != "$GITEA_RUNTIME_UID" ]; then
  owner="$(getent passwd "$GITEA_RUNTIME_UID" | cut -d: -f1 || true)"
  [ -z "$owner" ] || [ "$owner" = "$user" ] || { echo "runtime uid is already used by user $owner" >&2; exit 1; }
  usermod -u "$GITEA_RUNTIME_UID" "$user"
fi
home="$(getent passwd "$user" | cut -d: -f6)"
[ -z "$home" ] || chown -R "$GITEA_RUNTIME_UID:$GITEA_RUNTIME_GID" "$home"
`
	values := map[string]string{
		"GITEA_REMOTE_USER": environment.RemoteUser,
		"GITEA_RUNTIME_UID": strconv.FormatUint(uint64(runtimeUser.UID), 10),
		"GITEA_RUNTIME_GID": strconv.FormatUint(uint64(runtimeUser.GID), 10),
	}
	if _, _, err := e.exec(ctx, environment.PrimaryContainerID, "root", "/", []string{"/bin/sh", "-c", command}, values, nil); err != nil {
		return fmt.Errorf("update Dev Container remote user UID/GID: %w", err)
	}
	return nil
}

func (e *Engine) configureGit(ctx context.Context, environment *devcontainer.Environment, request devcontainer.RuntimeRequest) error {
	values := map[string]string{
		"GITEA_GIT_USER_NAME":  request.GitUserName,
		"GITEA_GIT_USER_EMAIL": request.GitUserEmail,
	}
	command := `set -eu
git config --global user.name "$GITEA_GIT_USER_NAME"
git config --global user.email "$GITEA_GIT_USER_EMAIL"
if [ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-credential ]; then
  git config --global credential.helper "!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential"
  git config credential.helper "!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential"
fi
if [ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-ssh ]; then
  git config core.sshCommand "/var/lib/gitea-codespace/bin/gitea-codespace-git-ssh"
fi`
	if _, _, err := e.exec(ctx, environment.PrimaryContainerID, environment.RemoteUser, environment.RemoteWorkdir, []string{"/bin/sh", "-c", command}, values, nil); err != nil {
		return fmt.Errorf("configure Git in Dev Container: %w", err)
	}
	return nil
}

func (e *Engine) startWebIDE(ctx context.Context, environment *devcontainer.Environment, secrets map[string]string) error {
	if err := e.RunPostAttach(ctx, environment, secrets); err != nil {
		return err
	}
	customizations := struct {
		VSCode struct {
			Settings   map[string]any `json:"settings"`
			Extensions []string       `json:"extensions"`
		} `json:"vscode"`
	}{}
	if raw := environment.Configuration.Customizations["vscode"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &customizations.VSCode); err != nil {
			return fmt.Errorf("decode VS Code customizations: %w", err)
		}
	}
	settings, err := json.Marshal(customizations.VSCode.Settings)
	if err != nil {
		return err
	}
	if len(settings) == 0 || string(settings) == "null" {
		settings = []byte("{}")
	}
	command := `set -eu
command -v code-server >/dev/null 2>&1 || { echo "code-server is not installed by the platform Feature" >&2; exit 1; }
settings_dir="${XDG_DATA_HOME:-$HOME/.local/share}/code-server/User"
mkdir -p "$settings_dir"
umask 077
cat > "$settings_dir/settings.json"
pid_file=/var/lib/gitea-codespace/runtime/code-server.pid
running=false
if [ -r "$pid_file" ]; then
  pid="$(cat "$pid_file")"
  if kill -0 "$pid" 2>/dev/null && grep -a -q code-server "/proc/$pid/cmdline" 2>/dev/null; then
    running=true
  fi
fi
if [ "$running" != true ]; then
  nohup code-server --auth none --bind-addr 0.0.0.0:13337 --disable-telemetry --disable-update-check "$GITEA_WORKSPACE" >/var/lib/gitea-codespace/runtime/code-server.log 2>&1 </dev/null &
  printf '%s\n' "$!" > "$pid_file"
fi`
	if _, _, err := e.exec(ctx, environment.PrimaryContainerID, environment.RemoteUser, environment.RemoteWorkdir, []string{"/bin/sh", "-c", command}, map[string]string{"GITEA_WORKSPACE": environment.RemoteWorkdir}, bytes.NewReader(settings)); err != nil {
		return fmt.Errorf("start platform Web IDE: %w", err)
	}
	for _, extension := range customizations.VSCode.Extensions {
		extension = strings.TrimSpace(extension)
		if extension == "" {
			return fmt.Errorf("VS Code extension identifier is empty")
		}
		if _, _, err := e.exec(ctx, environment.PrimaryContainerID, environment.RemoteUser, environment.RemoteWorkdir, []string{"code-server", "--install-extension", extension}, nil, nil); err != nil {
			fmt.Fprintf(e.stderr, "warning: install VS Code extension %s: %v\n", extension, err)
		}
	}
	return nil
}
