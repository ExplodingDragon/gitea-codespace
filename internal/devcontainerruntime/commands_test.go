// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"github.com/stretchr/testify/require"
)

func TestConfigureRuntimeCommands(t *testing.T) {
	var commands [][]string
	var installed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _, _ = io.Copy(io.Discard, r.Body) }()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = io.WriteString(w, `{"Os":"linux","Arch":"amd64","ApiVersion":"1.51"}`)
		case strings.HasSuffix(r.URL.Path, "/containers/container/exec"):
			var request struct{ Cmd []string }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			commands = append(commands, request.Cmd)
			_, _ = io.WriteString(w, `{"Id":"execution"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/execution/start"):
			_, _ = io.Copy(io.Discard, r.Body)
			conn, output, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprint(output, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = output.Flush()
		case strings.HasSuffix(r.URL.Path, "/exec/execution/json"):
			_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		case strings.HasSuffix(r.URL.Path, "/containers/container/archive"):
			header, err := tar.NewReader(r.Body).Next()
			if err != nil {
				t.Error(err)
				return
			}
			installed = append(installed, header.Name)
			if header.Mode != 0o755 {
				t.Errorf("command %s mode = %o", header.Name, header.Mode)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+server.Listener.Addr().String())
	t.Setenv("DOCKER_API_VERSION", "1.51")
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "")
	engine, err := containerdocker.New(t.Context(), io.Discard, io.Discard)
	require.NoError(t, err)
	defer engine.Close()
	state := &devcontainer.State{PrimaryContainerID: "container"}
	require.NoError(t, configureRuntimeCommands(t.Context(), engine, state, Request{
		DevContainer: Configuration{GiteaCLI: true}, USBIP: USBIPContainerConfiguration{Enabled: true},
	}))
	require.Equal(t, []string{"gitea-codespace-endpoint", "gitea-codespace-usbip", "gt"}, installed)
	require.Equal(t, []string{"/bin/sh", "-c", installGiteaCLIScript}, commands[len(commands)-1])
	installed, commands = nil, nil
	require.NoError(t, configureRuntimeCommands(t.Context(), engine, state, Request{}))
	require.Equal(t, []string{"gitea-codespace-endpoint"}, installed)
	require.Equal(t, []string{"rm", "-f", containerUSBIPBinary}, commands[len(commands)-1])
}

func TestGiteaCLIWrapperLoadsEnvironment(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	environmentPath := filepath.Join(directory, "env.sh")
	binaryPath := filepath.Join(directory, "gt-real")
	wrapperPath := filepath.Join(directory, "gt")
	require.NoError(t, os.WriteFile(environmentPath, []byte("export GITEA_INSTANCE_URL='https://gitea.example.com'\nexport GITEA_TOKEN='current-token'\nexport PROJECT_KEY='project-value'\n"), 0o600))
	require.NoError(t, os.WriteFile(binaryPath, []byte("#!/bin/sh\nprintf '%s\\000' \"$GITEA_INSTANCE_URL\" \"$GITEA_TOKEN\" \"$GITEA_CLI_NO_UPDATE_NOTIFIER\" \"$PROJECT_KEY\" \"$EXISTING_VALUE\" \"$@\"\ncat\nexit 23\n"), 0o755))
	wrapper := strings.NewReplacer(containerEnvironmentPath, environmentPath, platformGiteaCLIPath, binaryPath).Replace(string(giteaCLIWrapper))
	require.NoError(t, os.WriteFile(wrapperPath, []byte(wrapper), 0o755))
	for _, inheritedToken := range []string{"", "stale-token"} {
		command := exec.CommandContext(t.Context(), wrapperPath, "issue", "argument with spaces", "", "literal$argument")
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "EXISTING_VALUE=retained"}
		if inheritedToken != "" {
			command.Env = append(command.Env, "GITEA_TOKEN="+inheritedToken)
		}
		command.Stdin = strings.NewReader("stdin-data")
		output, err := command.Output()
		var exitError *exec.ExitError
		require.ErrorAs(t, err, &exitError)
		require.Equal(t, 23, exitError.ExitCode())
		require.Equal(t, "https://gitea.example.com\x00current-token\x001\x00project-value\x00retained\x00issue\x00argument with spaces\x00\x00literal$argument\x00stdin-data", string(output))
	}
	require.NoError(t, os.Remove(environmentPath))
	output, err := exec.CommandContext(t.Context(), wrapperPath, "version").CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), environmentPath)
}

func TestInstallGiteaCLICompletions(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "gt-real")
	require.NoError(t, os.WriteFile(binaryPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0o755))
	script := strings.NewReplacer(platformGiteaCLIPath, binaryPath, "/usr/local", directory).Replace(installGiteaCLIScript)
	for range 2 {
		command := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
		command.Env = []string{"PATH=" + os.Getenv("PATH")}
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		for name, path := range map[string]string{
			"bash": "share/bash-completion/completions/gt", "zsh": "share/zsh/site-functions/_gt", "fish": "share/fish/vendor_completions.d/gt.fish",
		} {
			content, err := os.ReadFile(filepath.Join(directory, path))
			require.NoError(t, err)
			require.Equal(t, "completion "+name+"\n", string(content))
		}
	}
}
