// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"gitea.dev/codespace/devcontainer"
)

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
}

func TestDockerProgressStreams(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	frames := strings.Repeat("{\"id\":\"layer\",\"status\":\"Downloading\",\"progress\":\"[====]\",\"progressDetail\":{\"current\":1,\"total\":2}}\n", 100)
	if err := streamDockerPullProgress(strings.NewReader(frames), &output); err != nil {
		t.Fatalf("stream pull progress: %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("pull progress output = %q", output.String())
	}

	if err := streamDockerPullProgress(strings.NewReader("{\"errorDetail\":{\"message\":\"pull denied\"}}\n"), &output); err == nil || err.Error() != "pull denied" {
		t.Fatalf("pull error = %v", err)
	}
}
