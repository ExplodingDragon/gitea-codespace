// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitea.dev/codespace/devcontainer"
)

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

	if err := streamDockerBuildOutput(strings.NewReader("{\"stream\":\"RUN echo ready\\n\"}\n"), &output); err != nil {
		t.Fatalf("stream build output: %v", err)
	}
	if output.String() != "RUN echo ready\n" {
		t.Fatalf("build output = %q", output.String())
	}

	if err := streamDockerPullProgress(strings.NewReader("{\"errorDetail\":{\"message\":\"pull denied\"}}\n"), &output); err == nil || err.Error() != "pull denied" {
		t.Fatalf("pull error = %v", err)
	}
}
