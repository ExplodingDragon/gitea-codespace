// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/codespace/devcontainer"
)

func TestExtractFeatureLayerAcceptsArchiveRoot(t *testing.T) {
	t.Parallel()

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatalf("write Feature archive root: %v", err)
	}
	content := []byte("#!/bin/sh\n")
	if err := writer.WriteHeader(&tar.Header{Name: "./install.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatalf("write Feature archive file header: %v", err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatalf("write Feature archive file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close Feature archive: %v", err)
	}

	directory := t.TempDir()
	if err := extractFeatureLayer(bytes.NewReader(archive.Bytes()), directory); err != nil {
		t.Fatalf("extract Feature archive: %v", err)
	}
	extracted, err := os.ReadFile(filepath.Join(directory, "install.sh"))
	if err != nil {
		t.Fatalf("read extracted Feature file: %v", err)
	}
	if !bytes.Equal(extracted, content) {
		t.Fatalf("extracted Feature file = %q", extracted)
	}
}

func TestResolveFeatureOptionsAndMergeMetadata(t *testing.T) {
	t.Parallel()

	options, err := resolveFeatureOptions(map[string]featureOption{
		"port":    {Type: "string", Default: json.RawMessage(`"8080"`)},
		"enabled": {Type: "boolean", Default: json.RawMessage(`false`)},
	}, json.RawMessage(`{"port":"13337","enabled":true}`))
	if err != nil {
		t.Fatalf("resolve Feature options: %v", err)
	}
	if options["PORT"] != "13337" || options["ENABLED"] != "true" {
		t.Fatalf("Feature options = %#v", options)
	}

	configuration := devcontainer.Merge(devcontainer.Configuration{}, featureConfigurationFromMetadata(featureMetadata{
		ID:             "editor",
		RemoteUser:     "developer",
		ContainerEnv:   map[string]string{"PATH_PREFIX": "/tools"},
		Customizations: map[string]json.RawMessage{"vscode": json.RawMessage(`{"extensions":["example.tool"]}`)},
	}))
	if configuration.RemoteUser != "developer" || configuration.ContainerEnv["PATH_PREFIX"] != "/tools" || len(configuration.Customizations["vscode"]) == 0 {
		t.Fatalf("merged Feature metadata = %#v", configuration)
	}
}

func TestMergeFeatureLifecycleCommandsFlattensObjects(t *testing.T) {
	t.Parallel()

	configuration := devcontainer.Configuration{
		PostCreateCommand: devcontainer.Command{Value: json.RawMessage(`{"repository":"echo repository"}`)},
	}
	configuration = devcontainer.Merge(featureConfigurationFromMetadata(featureMetadata{
		ID:                "first",
		PostCreateCommand: devcontainer.Command{Value: json.RawMessage(`{"install":"echo first"}`)},
	}), configuration)
	configuration = devcontainer.Merge(featureConfigurationFromMetadata(featureMetadata{
		ID:                "second",
		PostCreateCommand: devcontainer.Command{Value: json.RawMessage(`{"install":"echo second"}`)},
	}), configuration)

	var commands map[string]json.RawMessage
	if err := json.Unmarshal(configuration.PostCreateCommand.Value, &commands); err != nil {
		t.Fatalf("decode merged lifecycle commands: %v", err)
	}
	want := map[string]bool{`"echo second"`: false, `"echo first"`: false, `"echo repository"`: false}
	if len(commands) != len(want) {
		t.Fatalf("merged lifecycle commands = %s", configuration.PostCreateCommand.Value)
	}
	for _, value := range commands {
		want[string(value)] = true
	}
	for value, found := range want {
		if !found {
			t.Fatalf("merged lifecycle command %s is missing from %s", value, configuration.PostCreateCommand.Value)
		}
	}
}

func TestOrderFeaturesUsesDependenciesAndSoftInstallHints(t *testing.T) {
	t.Parallel()

	features := map[string]*resolvedFeature{
		"ghcr.io/example/base:1": {reference: "ghcr.io/example/base:1", metadata: featureMetadata{ID: "base"}},
		"ghcr.io/example/tool:1": {
			reference: "ghcr.io/example/tool:1",
			metadata:  featureMetadata{ID: "tool", DependsOn: map[string]json.RawMessage{"ghcr.io/example/base:1": json.RawMessage(`{}`)}},
		},
		"ghcr.io/example/editor:1": {
			reference: "ghcr.io/example/editor:1",
			metadata:  featureMetadata{ID: "editor", InstallsAfter: []string{"ghcr.io/example/tool:1"}},
		},
	}
	ordered, err := orderFeatures(features, nil)
	if err != nil {
		t.Fatalf("order Features: %v", err)
	}
	positions := map[string]int{}
	for index, feature := range ordered {
		positions[feature.metadata.ID] = index
	}
	if positions["base"] > positions["tool"] || positions["tool"] > positions["editor"] {
		t.Fatalf("Feature order = %#v", positions)
	}

	features["ghcr.io/example/base:1"].metadata.InstallsAfter = []string{"ghcr.io/example/editor:1"}
	if _, err := orderFeatures(features, nil); err != nil {
		t.Fatalf("soft installsAfter cycle rejected: %v", err)
	}
}
