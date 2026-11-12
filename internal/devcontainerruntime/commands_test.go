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
	"strings"
	"testing"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"github.com/stretchr/testify/require"
)

func TestConfigureRuntimeCommandsFollowsUSBIPCapability(t *testing.T) {
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
	require.NoError(t, configureRuntimeCommands(t.Context(), engine, state, true))
	require.Equal(t, []string{"gitea-codespace-endpoint", "gitea-codespace-usbip"}, installed)
	installed, commands = nil, nil
	require.NoError(t, configureRuntimeCommands(t.Context(), engine, state, false))
	require.Equal(t, []string{"gitea-codespace-endpoint"}, installed)
	require.Equal(t, []string{"rm", "-f", containerUSBIPBinary}, commands[len(commands)-1])
}
