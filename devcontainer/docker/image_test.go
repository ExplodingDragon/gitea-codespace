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
	"time"

	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"gitea.dev/codespace/devcontainer"
)

func TestResolveAndPullImage(t *testing.T) {
	for _, scenario := range []string{"mirror", "fallback", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			pulls := make(chan string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/_ping":
					w.Header().Set("OSType", "linux")
				case strings.HasSuffix(r.URL.Path, "/version"):
					_, _ = io.WriteString(w, `{"Os":"linux","Arch":"amd64","ApiVersion":"1.51"}`)
				case strings.HasSuffix(r.URL.Path, "/images/create"):
					reference := r.URL.Query().Get("fromImage")
					pulls <- reference
					if reference == "cache.example.com/proxy/app" && scenario != "mirror" {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = io.WriteString(w, `{"message":"mirror unavailable"}`)
					} else if scenario == "unavailable" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, `{"message":"upstream denied"}`)
					} else {
						_, _ = io.WriteString(w, `{}`)
					}
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
			var output bytes.Buffer
			engine, err := New(t.Context(), &output, &output)
			require.NoError(t, err)
			defer engine.Close()
			result, err := engine.resolveAndPullImage(t.Context(), "registry.example.com/app:latest", devcontainer.CacheOptions{
				Mirrors: map[string]string{"registry.example.com": "https://cache.example.com/proxy"},
			})
			close(pulls)
			var requests []string
			for reference := range pulls {
				requests = append(requests, reference)
			}
			if scenario == "mirror" {
				require.NoError(t, err)
				require.Equal(t, "cache.example.com/proxy/app:latest", result)
				require.Equal(t, []string{"cache.example.com/proxy/app"}, requests)
				require.Equal(t, "[INFO] [build] Pulling image registry.example.com/app:latest through mirror cache.example.com/proxy/app:latest\n"+
					"[INFO] [build] Pulled image registry.example.com/app:latest through mirror cache.example.com/proxy/app:latest\n", output.String())
				return
			}
			require.Equal(t, []string{"cache.example.com/proxy/app", "registry.example.com/app"}, requests)
			require.Contains(t, output.String(), "falling back to the original registry: pull Dev Container image cache.example.com/proxy/app:latest:")
			require.Contains(t, output.String(), "mirror unavailable")
			if scenario == "unavailable" {
				require.ErrorContains(t, err, "upstream denied")
			} else {
				require.NoError(t, err)
				require.Equal(t, "registry.example.com/app:latest", result)
			}
		})
	}
}

func TestPrepareImage(t *testing.T) {
	t.Parallel()
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/images/base/json") {
			t.Errorf("unexpected Docker request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Id":"base","Os":"linux","Architecture":"amd64","Config":{"User":"image-user","Labels":{
			"devcontainer.metadata":"[{\"containerEnv\":{\"ORDER\":\"image\"},\"entrypoint\":\"/image.sh\",\"onCreateCommand\":\"echo image\"}]"
		}}}`)
	}))
	defer server.Close()
	apiClient, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.51"))
	require.NoError(t, err)
	defer apiClient.Close()
	compose := &recordingBuildCompose{}
	engine := &Engine{client: apiClient, compose: compose, stdout: io.Discard, stderr: io.Discard}
	image, digests, err := engine.prepareImage(t.Context(), "base", resolved, devcontainer.HostUser{UID: 1001, GID: 1001}, func(image ImageCustomization) error {
		require.Equal(t, "devcontainer-user:test", image.BaseImage)
		require.Equal(t, "developer", image.ContainerUser)
		return os.WriteFile(filepath.Join(image.ContextDir, "Dockerfile"), []byte("FROM "+image.BaseImage+"\n"), 0o600)
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "devcontainer-customized:test", image)
	require.Contains(t, digests, "./feature")
	require.Len(t, compose.builds, 3)
	require.Equal(t, "Dockerfile.features", compose.builds[0].Dockerfile)
	require.Contains(t, filepath.Base(compose.builds[1].Context), "devcontainer-user-")
	require.Contains(t, filepath.Base(compose.builds[2].Context), "devcontainer-customization-")
	require.Equal(t, "repository", resolved.ContainerEnv["ORDER"])
	require.Equal(t, "true", resolved.ContainerEnv["FROM_FEATURE"])
	require.Equal(t, []string{"/image.sh", "/feature.sh"}, resolved.FeatureEntrypoints)
	require.Equal(t, []devcontainer.Command{
		{Value: json.RawMessage(`"echo image"`)}, {Value: json.RawMessage(`"echo feature"`)}, {Value: json.RawMessage(`"echo repository"`)},
	}, resolved.LifecycleCommands.OnCreate)
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

func TestImageMetadataContainsOnlyRuntimeProperties(t *testing.T) {
	t.Parallel()

	entry := imageMetadataEntry{
		Entrypoint:      "/usr/local/share/entrypoint.sh",
		ContainerEnv:    map[string]string{"FROM_IMAGE": "true"},
		OnCreateCommand: devcontainer.Command{Value: json.RawMessage(`"echo image"`)},
	}
	configuration := entry.configuration()
	if configuration.Image != "" || configuration.Build != nil || len(configuration.DockerComposeFile) != 0 || len(configuration.Features) != 0 {
		t.Fatalf("image metadata selected a configuration source: %#v", configuration)
	}
	if configuration.ContainerEnv["FROM_IMAGE"] != "true" || len(configuration.OnCreateCommand.Value) == 0 {
		t.Fatalf("image runtime metadata = %#v", configuration)
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
