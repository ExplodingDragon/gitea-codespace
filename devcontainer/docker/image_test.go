// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"gitea.dev/codespace/devcontainer"
)

type registryTestTransport func(*http.Request) (*http.Response, error)

func (f registryTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestResolveAndPullImage(t *testing.T) {
	for _, scenario := range []string{"without cache", "mirror", "fallback", "unavailable", "stalled response", "repeated progress", "progress", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("DOCKER_CONFIG", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var requests []string
				transport := registryTestTransport(func(r *http.Request) (*http.Response, error) {
					require.True(t, strings.HasSuffix(r.URL.Path, "/images/create"))
					reference := r.URL.Query().Get("fromImage")
					requests = append(requests, reference)
					response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("{}\n"))}
					if reference == "cache.example.com/proxy/app" {
						switch scenario {
						case "canceled", "stalled response":
							if scenario == "canceled" {
								cancel()
							}
							<-r.Context().Done()
							return nil, r.Context().Err()
						case "repeated progress", "progress":
							reader, writer := io.Pipe()
							response.Body = reader
							go func() {
								defer writer.Close()
								stop := context.AfterFunc(r.Context(), func() { _ = writer.CloseWithError(r.Context().Err()) })
								defer stop()
								encoder := json.NewEncoder(writer)
								message := dockerProgressMessage{ID: "layer", Status: "Downloading"}
								message.ProgressDetail.Current = 1
								_ = encoder.Encode(message)
								for range 3 {
									select {
									case <-r.Context().Done():
										return
									case <-time.After(imageTransferIdleTimeout / 2):
									}
									if scenario == "progress" {
										message.ProgressDetail.Current++
									}
									if err := encoder.Encode(message); err != nil {
										return
									}
								}
							}()
						case "fallback", "unavailable":
							response.StatusCode = http.StatusServiceUnavailable
							response.Body = io.NopCloser(strings.NewReader(`{"message":"mirror unavailable"}`))
						}
					} else if scenario == "unavailable" {
						response.StatusCode = http.StatusForbidden
						response.Body = io.NopCloser(strings.NewReader(`{"message":"upstream denied"}`))
					}
					return response, nil
				})
				apiClient, err := client.NewClientWithOpts(client.WithVersion("1.51"), client.WithHTTPClient(&http.Client{Transport: transport}))
				require.NoError(t, err)
				defer apiClient.Close()
				cli, err := command.NewDockerCli(command.WithAPIClient(apiClient))
				require.NoError(t, err)
				require.NoError(t, cli.Initialize(flags.NewClientOptions()))
				var output bytes.Buffer
				engine := &Engine{client: apiClient, cli: cli, stdout: &output, stderr: &output}
				start := time.Now()
				var cache devcontainer.CacheOptions
				if scenario != "without cache" {
					cache.Mirrors = map[string]string{"registry.example.com": "https://cache.example.com/proxy"}
				}
				result, err := engine.resolveAndPullImage(ctx, "registry.example.com/app:latest", cache)
				if scenario == "canceled" {
					require.ErrorIs(t, err, context.Canceled)
					require.Equal(t, []string{"cache.example.com/proxy/app"}, requests)
					return
				}
				if scenario == "mirror" || scenario == "progress" {
					require.NoError(t, err)
					require.Equal(t, "cache.example.com/proxy/app:latest", result)
					require.Equal(t, []string{"cache.example.com/proxy/app"}, requests)
					if scenario == "progress" {
						require.Greater(t, time.Since(start), imageTransferIdleTimeout)
					}
					return
				}
				if scenario == "without cache" {
					require.Equal(t, []string{"registry.example.com/app"}, requests)
				} else {
					require.Equal(t, []string{"cache.example.com/proxy/app", "registry.example.com/app"}, requests)
				}
				if scenario == "stalled response" || scenario == "repeated progress" {
					require.Equal(t, imageTransferIdleTimeout, time.Since(start))
					require.Contains(t, output.String(), "no progress")
				}
				if scenario == "unavailable" {
					require.ErrorContains(t, err, "upstream denied")
				} else {
					require.NoError(t, err)
					require.Equal(t, "registry.example.com/app:latest", result)
				}
			})
		})
	}
}

func TestResolveDigestImage(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const original = "registry.example.com/app@" + digest
	const mirrored = "cache.example.com/proxy/app@" + digest
	const localImage = `{"Id":"sha256:child","Os":"linux","Architecture":"amd64","Config":{}}`
	for _, test := range []struct {
		name      string
		image     string
		mirror    bool
		local     map[string]string
		wantImage string
		wantPulls []string
		wantError string
	}{
		{name: "local digest", image: original, local: map[string]string{original: localImage}, wantImage: original},
		{name: "tag and digest", image: "registry.example.com/app:release@" + digest, local: map[string]string{"registry.example.com/app:release@" + digest: localImage}, wantImage: "registry.example.com/app:release@" + digest},
		{name: "original before mirror", image: original, mirror: true, local: map[string]string{original: localImage, mirrored: localImage}, wantImage: original},
		{name: "local mirror", image: original, mirror: true, local: map[string]string{mirrored: localImage}, wantImage: mirrored},
		{name: "missing", image: original, wantImage: original, wantPulls: []string{"registry.example.com/app"}},
		{name: "missing with mirror", image: original, mirror: true, wantImage: mirrored, wantPulls: []string{"cache.example.com/proxy/app"}},
		{name: "other platform", image: original, local: map[string]string{original: `{"Os":"linux","Architecture":"arm64","Config":{}}`}, wantImage: original, wantPulls: []string{"registry.example.com/app"}},
		{name: "index", image: original, local: map[string]string{
			original: `{"Manifests":[{"ID":"sha256:child","Available":true,"Kind":"image","ImageData":{"Platform":{"os":"linux","architecture":"amd64"}}}]}`,
		}, wantImage: original},
		{name: "incomplete layers", image: original, local: map[string]string{
			original: `{"Os":"linux","Architecture":"amd64","Config":{},"Manifests":[{"ID":"sha256:child","Available":false,"Kind":"image","ImageData":{"Platform":{"os":"linux","architecture":"amd64"}}}]}`,
		}, wantImage: original, wantPulls: []string{"registry.example.com/app"}},
		{name: "inspect failure", image: original, wantError: "local inspection failed"},
		{name: "canceled", image: original, wantError: context.Canceled.Error()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DOCKER_CONFIG", t.TempDir())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var pulls []string
			transport := registryTestTransport(func(r *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, "{}"
				if strings.HasSuffix(r.URL.Path, "/json") {
					require.Equal(t, "1", r.URL.Query().Get("manifests"))
					name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/images/"), "/json")
					if test.name == "canceled" {
						cancel()
						return nil, ctx.Err()
					}
					if test.wantError != "" {
						status, body = http.StatusInternalServerError, `{"message":"local inspection failed"}`
					} else if value, exists := test.local[name]; exists {
						body = value
					} else {
						status, body = http.StatusNotFound, `{"message":"image not found"}`
					}
				} else {
					require.True(t, strings.HasSuffix(r.URL.Path, "/images/create"), r.URL.Path)
					require.Equal(t, digest, r.URL.Query().Get("tag"))
					pulls = append(pulls, r.URL.Query().Get("fromImage"))
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			apiClient, err := client.NewClientWithOpts(client.WithVersion("1.51"), client.WithHTTPClient(&http.Client{Transport: transport}))
			require.NoError(t, err)
			defer apiClient.Close()
			cli, err := command.NewDockerCli(command.WithAPIClient(apiClient))
			require.NoError(t, err)
			require.NoError(t, cli.Initialize(flags.NewClientOptions()))
			engine := &Engine{client: apiClient, cli: cli, platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"}, stdout: io.Discard, stderr: io.Discard}
			var cache devcontainer.CacheOptions
			if test.mirror {
				cache.Mirrors = map[string]string{"registry.example.com": "https://cache.example.com/proxy"}
			}
			result, err := engine.resolveAndPullImage(ctx, test.image, cache)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
				if test.name == "canceled" {
					require.ErrorIs(t, err, context.Canceled)
				}
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, test.wantImage, result)
			require.Equal(t, test.wantPulls, pulls)
		})
	}
}

func TestPrepareImage(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"build", "local cache"} {
		t.Run(scenario, func(t *testing.T) {
			workspace := t.TempDir()
			featureDir := filepath.Join(workspace, "feature")
			require.NoError(t, os.Mkdir(featureDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(featureDir, "devcontainer-feature.json"), []byte(`{
		"id":"example","version":"1.0.0","name":"Example",
		"containerEnv":{"FROM_FEATURE":"true"},"entrypoint":"/feature.sh","onCreateCommand":"echo feature"
	}`), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(featureDir, "install.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700))
			resolved := &devcontainer.ResolvedConfiguration{
				Configuration: devcontainer.Configuration{
					Image: "base", ContainerUser: "developer", RemoteUser: "remote",
					Features:        map[string]json.RawMessage{"./feature": json.RawMessage(`{}`)},
					ContainerEnv:    map[string]string{"ORDER": "repository"},
					OnCreateCommand: devcontainer.Command{Value: json.RawMessage(`"echo repository"`)},
				},
				Workspace: workspace, ConfigurationDir: workspace, ConfigurationPath: filepath.Join(workspace, "devcontainer.json"),
				AllowedPathRoot: workspace, DevContainerID: "test", Synthetic: true,
			}
			if scenario == "local cache" {
				resolved.Cache = devcontainer.CacheOptions{BuildRegistry: "https://registry.example.com/cache", BuildScope: "test"}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "/images/registry.example.com/cache/") && scenario == "local cache" {
					_, _ = io.WriteString(w, `{"Id":"features","Os":"linux","Architecture":"amd64","Config":{}}`)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/images/base/json") {
					t.Errorf("unexpected Docker request: %s", r.URL.Path)
				}
				_, _ = io.WriteString(w, `{"Id":"base","Os":"linux","Architecture":"amd64","Config":{"User":"image-user","Labels":{
			"devcontainer.metadata":"[{\"containerEnv\":{\"ORDER\":\"image\"},\"entrypoint\":\"/image.sh\",\"onCreateCommand\":\"echo image\"}]"
		}}}`)
			}))
			defer server.Close()
			apiClient, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.51"))
			require.NoError(t, err)
			defer apiClient.Close()
			compose := &recordingBuildCompose{}
			engine := &Engine{client: apiClient, compose: compose, platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"}, stdout: io.Discard, stderr: io.Discard}
			image, digests, err := engine.prepareImage(t.Context(), "base", resolved, devcontainer.HostUser{UID: 1001, GID: 1001}, func(image ImageCustomization) error {
				require.Equal(t, "devcontainer-user:test", image.BaseImage)
				require.Equal(t, "developer", image.ContainerUser)
				return os.WriteFile(filepath.Join(image.ContextDir, "Dockerfile"), []byte("FROM "+image.BaseImage+"\n"), 0o600)
			}, nil)
			require.NoError(t, err)
			require.Equal(t, "devcontainer-customized:test", image)
			require.Contains(t, digests, "./feature")
			if scenario == "local cache" {
				require.Len(t, compose.builds, 2)
			} else {
				require.Len(t, compose.builds, 3)
				require.Equal(t, "Dockerfile.features", compose.builds[0].Dockerfile)
			}
			require.Contains(t, filepath.Base(compose.builds[len(compose.builds)-2].Context), "devcontainer-user-")
			require.Contains(t, filepath.Base(compose.builds[len(compose.builds)-1].Context), "devcontainer-customization-")
			require.Equal(t, "repository", resolved.ContainerEnv["ORDER"])
			require.Equal(t, "true", resolved.ContainerEnv["FROM_FEATURE"])
			require.Equal(t, []string{"/image.sh", "/feature.sh"}, resolved.FeatureEntrypoints)
			require.Equal(t, []devcontainer.Command{
				{Value: json.RawMessage(`"echo image"`)}, {Value: json.RawMessage(`"echo feature"`)}, {Value: json.RawMessage(`"echo repository"`)},
			}, resolved.LifecycleCommands.OnCreate)
		})
	}
}

func TestInspectImageSelectsDockerServerPlatform(t *testing.T) {
	t.Parallel()

	wanted := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/images/base/json") {
			if request.URL.Query().Get("manifests") != "1" {
				t.Errorf("manifests query = %q", request.URL.Query().Get("manifests"))
			}
			_, _ = response.Write([]byte(`{"Id":"sha256:index","Manifests":[{"ID":"sha256:child","Available":true,"Kind":"image","ImageData":{"Platform":{"os":"linux","architecture":"amd64"}}}]}`))
			return
		}
		if !strings.HasSuffix(request.URL.Path, "/images/sha256:child/json") {
			t.Errorf("image path = %q", request.URL.Path)
		}
		_, _ = response.Write([]byte(`{"Id":"sha256:child","Os":"linux","Architecture":"amd64","Config":{"Env":["PATH=/usr/bin"]}}`))
	}))
	defer server.Close()
	apiClient, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.51"))
	if err != nil {
		t.Fatalf("create Docker client: %v", err)
	}
	defer func() { _ = apiClient.Close() }()
	engine := &Engine{client: apiClient, platform: &wanted}
	inspect, err := engine.inspectImage(context.Background(), "base")
	if err != nil {
		t.Fatalf("inspect image: %v", err)
	}
	if inspect.ID != "sha256:child" || inspect.Config == nil || len(inspect.Config.Env) != 1 || inspect.Config.Env[0] != "PATH=/usr/bin" {
		t.Fatalf("image ID = %q", inspect.ID)
	}
}

func TestCustomizeImageRequiresDockerfileAndCleansContext(t *testing.T) {
	t.Parallel()

	var contextDir string
	engine := &Engine{}
	_, err := engine.customizeImage(t.Context(), "base", &devcontainer.ResolvedConfiguration{}, func(image ImageCustomization) error {
		contextDir = image.ContextDir
		if image.BaseImage != "base" || image.ContainerUser != "root" {
			t.Fatalf("image customization = %#v", image)
		}
		return nil
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "Dockerfile") {
		t.Fatalf("customize image error = %v", err)
	}
	if _, err := os.Stat(contextDir); !os.IsNotExist(err) {
		t.Fatalf("customization context remains after failure: %v", err)
	}
}

func TestImagePlatform(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		platform *ocispec.Platform
		want     string
	}{
		{name: "unavailable"},
		{name: "architecture", platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"}, want: "linux/amd64"},
		{name: "variant", platform: &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, want: "linux/arm64/v8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := (&Engine{platform: test.platform}).imagePlatform(); got != test.want {
				t.Fatalf("image platform = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDockerPullProgressAggregatesLayerUpdates(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC)
	progress := dockerPullProgress{}
	message := dockerProgressMessage{ID: "layer-one", Status: "Downloading"}
	message.ProgressDetail.Current = 10
	message.ProgressDetail.Total = 100
	if summary := progress.observe(message, start); summary != "" {
		t.Fatalf("initial summary = %q", summary)
	}
	for i := int64(11); i < 100; i++ {
		message.ProgressDetail.Current = i
		if summary := progress.observe(message, start.Add(time.Second)); summary != "" {
			t.Fatalf("early summary = %q", summary)
		}
	}
	message.ProgressDetail.Current = 100
	if summary := progress.observe(message, start.Add(dockerPullProgressInterval)); summary != "Image pull progress: 0/1 layers complete, 100B / 100B downloaded" {
		t.Fatalf("summary = %q", summary)
	}
	message.Status = "Pull complete"
	if summary := progress.observe(message, start.Add(2*dockerPullProgressInterval)); summary != "Image pull progress: 1/1 layers complete, 100B / 100B downloaded" {
		t.Fatalf("completed summary = %q", summary)
	}
}

func TestParseImageMetadataPreservesEntrypointOrder(t *testing.T) {
	t.Parallel()

	metadata, err := parseImageMetadata([]byte(`[
		{"id":"first","entrypoint":"/first.sh","containerEnv":{"ORDER":"first"},"image":"ignored"},
		{"id":"second","entrypoint":"/second.sh","containerEnv":{"ORDER":"second"},"dockerComposeFile":"ignored.yaml"}
	]`))
	if err != nil {
		t.Fatalf("parse image metadata: %v", err)
	}
	if len(metadata.Entrypoints) != 2 || metadata.Entrypoints[0] != "/first.sh" || metadata.Entrypoints[1] != "/second.sh" {
		t.Fatalf("image metadata entrypoints = %#v", metadata.Entrypoints)
	}
	if metadata.Configuration.ContainerEnv["ORDER"] != "second" || metadata.Configuration.Image != "" || len(metadata.Configuration.DockerComposeFile) != 0 {
		t.Fatalf("merged image metadata = %#v", metadata.Configuration)
	}
	if len(metadata.Sources) != 2 || metadata.Sources[0].ContainerEnv["ORDER"] != "first" || metadata.Sources[1].ContainerEnv["ORDER"] != "second" {
		t.Fatalf("image metadata sources = %#v", metadata.Sources)
	}
}

func TestReadDockerProgress(t *testing.T) {
	t.Parallel()

	var messages []dockerProgressMessage
	require.NoError(t, readDockerProgress(strings.NewReader(`
{"id":"layer","status":"Downloading","progressDetail":{"current":1,"total":2}}
{"id":"layer","status":"Pull complete"}
`), func(message dockerProgressMessage) { messages = append(messages, message) }))
	require.Len(t, messages, 2)
	require.Equal(t, "layer", messages[0].ID)
	require.EqualValues(t, 1, messages[0].ProgressDetail.Current)
	require.EqualValues(t, 2, messages[0].ProgressDetail.Total)
	require.Equal(t, "Pull complete", messages[1].Status)

	for _, frame := range []string{`{"errorDetail":{"message":"pull denied"}}`, `{"error":"pull denied"}`} {
		require.EqualError(t, readDockerProgress(strings.NewReader(frame), nil), "pull denied")
	}
}
