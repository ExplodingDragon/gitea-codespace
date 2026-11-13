// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/require"

	"gitea.dev/codespace/devcontainer"
)

func TestCopyContent(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"copy", "api error", "archive error"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			archives := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/containers/container/archive") || r.URL.Query().Get("path") != "/usr/local/bin" {
					t.Errorf("unexpected copy request: %s %s", r.Method, r.URL)
				}
				content, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				archives <- content
				if scenario == "api error" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `{"message":"read-only destination"}`)
				}
			}))
			defer server.Close()
			apiClient, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.51"))
			require.NoError(t, err)
			defer apiClient.Close()
			engine := &Engine{client: apiClient}
			target, content := "/usr/local/bin/helper", []byte("#!/bin/sh\nprintf '%s\\n' \"$VALUE\"\n")
			if scenario == "archive error" {
				target += "\x00"
			}
			err = engine.CopyContent(t.Context(), "container", target, 0o755, content)
			close(archives)
			if scenario == "archive error" {
				require.ErrorContains(t, err, "archive runtime helper")
				require.Empty(t, archives)
				return
			}
			if scenario == "api error" {
				require.ErrorContains(t, err, "read-only destination")
			} else {
				require.NoError(t, err)
			}
			archive := tar.NewReader(bytes.NewReader(<-archives))
			header, err := archive.Next()
			require.NoError(t, err)
			require.Equal(t, "helper", header.Name)
			require.Equal(t, int64(0o755), header.Mode)
			copied, err := io.ReadAll(archive)
			require.NoError(t, err)
			require.Equal(t, content, copied)
			_, err = archive.Next()
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

func TestInitializeCommandCanRunForCreateAndResume(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	command := devcontainer.Command{Value: []byte(`"printf x >> initialized"`)}
	user := devcontainer.HostUser{
		Name: os.Getenv("USER"), Home: os.Getenv("HOME"),
		UID: uint32(os.Geteuid()), GID: uint32(os.Getegid()),
	}
	for range 2 {
		if err := runInitializeCommand(t.Context(), command, user, directory, nil, io.Discard, io.Discard); err != nil {
			t.Fatalf("run initializeCommand: %v", err)
		}
	}
	content, err := os.ReadFile(filepath.Join(directory, "initialized"))
	if err != nil || string(content) != "xx" {
		t.Fatalf("initializeCommand output = %q, %v", content, err)
	}
}

func TestInitializeCommandUsesOnlyExplicitEnvironment(t *testing.T) {
	t.Setenv("CODESPACE_PRIVATE_TOKEN", "secret")
	directory := t.TempDir()
	command := devcontainer.Command{Value: []byte(`"printf '%s' \"${CODESPACE_PRIVATE_TOKEN-unset}:$SAFE_VALUE\" > environment"`)}
	user := devcontainer.HostUser{
		Name: os.Getenv("USER"), Home: os.Getenv("HOME"),
		UID: uint32(os.Geteuid()), GID: uint32(os.Getegid()),
	}
	if err := runInitializeCommand(t.Context(), command, user, directory, map[string]string{"SAFE_VALUE": "available"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("run initializeCommand: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "environment"))
	if err != nil || string(content) != "unset:available" {
		t.Fatalf("initializeCommand environment = %q, %v", content, err)
	}
}

func TestNamedLifecycleCommands(t *testing.T) {
	command := devcontainer.Command{Value: []byte(`{"install":"exit 17","configure":["/bin/sh","-c","exit 17"]}`)}
	commands, err := decodeCommand("postCreateCommand", command)
	require.NoError(t, err)
	require.Equal(t, []lifecycleCommand{
		{name: "postCreateCommand.configure", arguments: []string{"/bin/sh", "-c", "exit 17"}},
		{name: "postCreateCommand.install", arguments: []string{"/bin/sh", "-c", "exit 17"}},
	}, commands)
	var output bytes.Buffer
	user := devcontainer.HostUser{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	err = runInitializeCommand(t.Context(), command, user, t.TempDir(), nil, &output, io.Discard)
	require.ErrorContains(t, err, "run initializeCommand.")
	require.Contains(t, output.String(), "[INFO] [lifecycle] Running initializeCommand.configure")
	require.Contains(t, output.String(), "[INFO] [lifecycle] Running initializeCommand.install")
}

func execForTest(engine *Engine, ctx context.Context, containerID, user, workdir string, command []string, environment map[string]string) ([]byte, []byte, error) {
	return engine.exec(ctx, containerID, user, workdir, command, environment, nil, engine.stdout, engine.stderr, true)
}

func TestDockerExecOutputAndCancellation(t *testing.T) {
	for _, mode := range []string{"stream", "capture", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			attached := make(chan struct{})
			closed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/container/exec"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"Id":"execution"}`)
				case strings.HasSuffix(r.URL.Path, "/exec/execution/start"):
					conn, output, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer close(closed)
					defer func() { _ = conn.Close() }()
					_, _ = fmt.Fprint(output, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
					_ = output.Flush()
					close(attached)
					if mode == "cancel" {
						_, _ = io.Copy(io.Discard, conn)
					} else {
						_, _ = io.Copy(stdcopy.NewStdWriter(conn, stdcopy.Stdout), strings.NewReader(strings.Repeat("x", 2*maxExecCaptureBytes)))
					}
				case strings.HasSuffix(r.URL.Path, "/exec/execution/json"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			apiClient, err := client.NewClientWithOpts(client.WithHost("tcp://"+server.Listener.Addr().String()), client.WithVersion("1.51"))
			require.NoError(t, err)
			defer func() { _ = apiClient.Close() }()
			engine := &Engine{client: apiClient, stdout: io.Discard, stderr: io.Discard}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if mode == "capture" {
					_, _, err = execForTest(engine, ctx, "container", "root", "/", []string{"test"}, nil)
				} else {
					err = engine.Run(ctx, "container", "root", "/", []string{"test"}, nil, nil)
				}
				done <- err
			}()
			select {
			case <-attached:
			case err := <-done:
				t.Fatalf("Docker execution failed before attachment: %v", err)
			case <-ctx.Done():
				t.Fatal("Docker attachment did not start")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				switch mode {
				case "cancel":
					require.ErrorIs(t, err, context.Canceled)
				case "capture":
					require.ErrorContains(t, err, "output exceeds")
				default:
					require.NoError(t, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Docker output read remained blocked")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Docker connection was not closed")
			}
		})
	}
}

func TestDockerRunDetached(t *testing.T) {
	requests := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/container/exec"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Id":"execution"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/execution/start"):
			requests <- body
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	apiClient, err := client.NewClientWithOpts(client.WithHost("tcp://"+server.Listener.Addr().String()), client.WithVersion("1.51"))
	require.NoError(t, err)
	defer func() { _ = apiClient.Close() }()
	engine := &Engine{client: apiClient}
	require.NoError(t, engine.RunDetached(t.Context(), "container", "user", "/workspace", []string{"service"}, map[string]string{"NAME": "value"}))
	var options container.ExecStartOptions
	select {
	case body := <-requests:
		require.NoError(t, json.Unmarshal(body, &options))
	case <-time.After(time.Second):
		t.Fatal("detached Docker execution did not start")
	}
	require.True(t, options.Detach)
}

func TestRemoteUserIDs(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		uid    int
		gid    int
	}{
		{name: "root", output: "0\n0\n"},
		{name: "image user", output: "2001\n3001\n", uid: 2001, gid: 3001},
		{name: "invalid", output: "4294967295\n3001\n", uid: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var execution container.ExecOptions
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/container/exec"):
					if err := json.NewDecoder(r.Body).Decode(&execution); err != nil {
						t.Error(err)
					}
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
					_, _ = io.WriteString(stdcopy.NewStdWriter(conn, stdcopy.Stdout), test.output)
				case strings.HasSuffix(r.URL.Path, "/exec/execution/json"):
					_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			apiClient, err := client.NewClientWithOpts(client.WithHost("tcp://"+server.Listener.Addr().String()), client.WithVersion("1.51"))
			require.NoError(t, err)
			defer apiClient.Close()
			engine := &Engine{client: apiClient, stdout: io.Discard, stderr: io.Discard}
			uid, gid, err := engine.RemoteUserIDs(t.Context(), &devcontainer.State{PrimaryContainerID: "container", RemoteUser: "developer:developers"})
			if test.uid < 0 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.uid, uid)
				require.Equal(t, test.gid, gid)
			}
			require.Equal(t, "developer:developers", execution.User)
			require.Equal(t, "/", execution.WorkingDir)
			require.Equal(t, []string{"/bin/sh", "-c", "id -u && id -g"}, execution.Cmd)
		})
	}
}
