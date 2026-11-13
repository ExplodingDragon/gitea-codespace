// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gitea.dev/codespace/devcontainer"
	"github.com/stretchr/testify/require"
)

func TestCleanupCompletesAfterCancellation(t *testing.T) {
	for _, action := range []string{"stop", "delete"} {
		t.Run(action, func(t *testing.T) {
			var stopped atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/_ping":
					w.Header().Set("OSType", "linux")
				case strings.HasSuffix(r.URL.Path, "/version"):
					_, _ = fmt.Fprint(w, `{"Os":"linux","Arch":"amd64","ApiVersion":"1.51"}`)
				case strings.HasSuffix(r.URL.Path, "/containers/container/stop"):
					stopped.Store(true)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+server.Listener.Addr().String())
			t.Setenv("DOCKER_API_VERSION", "1.51")
			t.Setenv("DOCKER_CONFIG", t.TempDir())
			t.Setenv("DOCKER_CONTEXT", "")
			state := &devcontainer.State{Version: devcontainer.StateFormatVersion, ID: "environment", OwnerID: "runtime", PrimaryContainerID: "container", ConfigurationPath: "/workspace/devcontainer.json", ConfigurationSHA256: strings.Repeat("a", 64), Workspace: "/workspace", WorkspaceFolder: "/workspace", RemoteWorkdir: "/workspace", RemoteUser: "root"}
			var output bytes.Buffer
			closedAccess := false
			lifecycleCalled := false
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			outcome, err := Apply(ctx, Request{
				Action: action, CodespaceUUID: "runtime", OperationVersion: 2, Environment: state,
				DevContainer: Configuration{LifecycleScript: "exit 1"},
				RunTemplateLifecycle: func(context.Context, string, func(context.Context) error) error {
					lifecycleCalled = true
					return errors.New("stop hook failed")
				},
				BeforeContainerStop: func() {
					closedAccess = true
					cancel()
				},
			}, nil, &output)
			require.NoError(t, err)
			require.Same(t, state, outcome.Environment)
			require.True(t, stopped.Load())
			require.True(t, closedAccess)
			require.Equal(t, action == "stop", lifecycleCalled)
			if action == "stop" {
				require.Contains(t, output.String(), "stop hook failed")
			}
		})
	}
}
