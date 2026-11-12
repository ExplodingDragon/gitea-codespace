// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	composeapi "github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"

	"gitea.dev/codespace/devcontainer"
)

type creationCleanupClient struct {
	client.APIClient
	t          *testing.T
	removeErr  error
	removeCall int
}

func (c *creationCleanupClient) ContainerRemove(ctx context.Context, id string, options container.RemoveOptions) error {
	c.removeCall++
	require.NoError(c.t, ctx.Err())
	deadline, ok := ctx.Deadline()
	require.True(c.t, ok, "creation cleanup must have a deadline")
	require.Positive(c.t, time.Until(deadline))
	require.LessOrEqual(c.t, time.Until(deadline), time.Minute)
	if c.removeErr != nil {
		return c.removeErr
	}
	return c.APIClient.ContainerRemove(ctx, id, options)
}

type creationCompose struct {
	composeapi.Compose
	client   client.APIClient
	upErr    error
	downCall int
}

func (c *creationCompose) Up(ctx context.Context, project *composetypes.Project, _ composeapi.UpOptions) error {
	_, err := c.client.ContainerCreate(ctx, &container.Config{Labels: project.Services["workspace"].Labels}, nil, nil, nil, "workspace")
	return errors.Join(err, c.upErr)
}

func (*creationCompose) Ps(context.Context, string, composeapi.PsOptions) ([]composeapi.ContainerSummary, error) {
	return nil, nil
}

func (c *creationCompose) Down(ctx context.Context, _ string, options composeapi.DownOptions) error {
	c.downCall++
	if options.Volumes {
		return errors.New("creation cleanup must preserve volumes")
	}
	return c.client.ContainerRemove(ctx, "container", container.RemoveOptions{Force: true})
}

func TestCreateResourceCleanup(t *testing.T) {
	for _, scenario := range []string{"start", "runArgs", "prepare", "cancel", "cleanup error", "compose start", "compose empty", "success"} {
		t.Run(scenario, func(t *testing.T) {
			workspace := t.TempDir()
			configuration := map[string]any{"image": "alpine:latest", "remoteUser": "root", "userEnvProbe": "none"}
			useCompose := strings.HasPrefix(scenario, "compose")
			if useCompose {
				delete(configuration, "image")
				configuration["dockerComposeFile"], configuration["service"] = "compose.yaml", "workspace"
				require.NoError(t, os.WriteFile(filepath.Join(workspace, "compose.yaml"), []byte("services:\n  workspace:\n    image: alpine:latest\n"), 0o600))
			} else if scenario == "runArgs" {
				configuration["runArgs"] = []string{"--hostname", "workspace"}
			}
			data, err := json.Marshal(configuration)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(workspace, "devcontainer.json"), data, 0o600))

			var mu sync.Mutex
			var exists bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				defer func() { _, _ = io.Copy(io.Discard, r.Body) }()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/_ping":
					w.Header().Set("OSType", "linux")
				case strings.HasSuffix(r.URL.Path, "/version"):
					_, _ = io.WriteString(w, `{"Os":"linux","Arch":"amd64","ApiVersion":"1.51"}`)
				case strings.HasSuffix(r.URL.Path, "/containers/json"):
					if exists {
						_, _ = io.WriteString(w, `[{"Id":"container"}]`)
					} else {
						_, _ = io.WriteString(w, `[]`)
					}
				case strings.HasSuffix(r.URL.Path, "/networks"):
					_, _ = io.WriteString(w, `[]`)
				case strings.HasSuffix(r.URL.Path, "/volumes"):
					_, _ = io.WriteString(w, `{"Volumes":[]}`)
				case strings.HasSuffix(r.URL.Path, "/images/create"):
					_, _ = io.WriteString(w, `{}`)
				case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
					_, _ = io.WriteString(w, `{"Id":"image","Os":"linux","Architecture":"amd64","Config":{"User":"root"}}`)
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					exists = true
					_, _ = io.WriteString(w, `{"Id":"container"}`)
				case strings.HasSuffix(r.URL.Path, "/containers/container/start"):
					if scenario == "start" || scenario == "runArgs" {
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(w, `{"message":"start failed"}`)
					}
				case strings.HasSuffix(r.URL.Path, "/containers/container/json"):
					_, _ = io.WriteString(w, `{"Id":"container","State":{"Running":true},"Config":{"User":"root"}}`)
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/container"):
					exists = false
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+server.Listener.Addr().String())
			t.Setenv("DOCKER_API_VERSION", "1.51")
			t.Setenv("DOCKER_CONFIG", t.TempDir())
			t.Setenv("DOCKER_CONTEXT", "")
			engine, err := New(t.Context(), io.Discard, io.Discard)
			require.NoError(t, err)
			defer engine.Close()
			cleanup := &creationCleanupClient{APIClient: engine.client, t: t}
			engine.client = cleanup
			compose := &creationCompose{client: cleanup}
			engine.compose = compose
			failure, cleanupFailure := errors.New("preparation failed"), errors.New("cleanup failed")
			if scenario == "compose start" {
				compose.upErr = failure
			}
			if scenario == "cleanup error" {
				cleanup.removeErr = cleanupFailure
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			state, err := engine.Create(ctx, CreateOptions{
				OwnerID: "owner", Workspace: workspace, Source: devcontainer.Source{Path: "devcontainer.json"},
				PrepareLifecycle: func(context.Context, *Engine, *devcontainer.State) error {
					if scenario == "success" {
						return nil
					}
					if scenario == "cancel" {
						cancel()
					}
					return failure
				},
			})
			if scenario == "success" {
				require.NoError(t, err)
				require.NotNil(t, state)
				require.Zero(t, cleanup.removeCall)
				return
			}
			require.Nil(t, state)
			switch scenario {
			case "start", "runArgs":
				require.ErrorContains(t, err, "start failed")
			case "compose empty":
				require.ErrorContains(t, err, `service "workspace" did not create a container`)
			default:
				require.ErrorIs(t, err, failure)
			}
			if scenario == "cleanup error" {
				require.ErrorIs(t, err, cleanupFailure)
			}
			require.Equal(t, 1, cleanup.removeCall)
			if useCompose {
				require.Equal(t, 1, compose.downCall)
			}
		})
	}
}
