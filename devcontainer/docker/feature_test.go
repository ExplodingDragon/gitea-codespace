// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/codespace/devcontainer"
)

func TestFeatureRegistryClientUsesConfiguredRootCA(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	root := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := featureRegistryClient(root)
	if err != nil {
		t.Fatalf("create Feature registry client: %v", err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("create Feature registry request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request Feature registry with configured CA: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close Feature registry response: %v", err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("Feature registry status = %s", response.Status)
	}
	if _, err := featureRegistryClient([]byte("not a certificate")); err == nil {
		t.Fatal("invalid Feature registry CA was accepted")
	}
}

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

func TestLoadLocalFeature(t *testing.T) {
	t.Parallel()

	configurationDirectory := t.TempDir()
	featureDirectory := filepath.Join(configurationDirectory, "feature")
	if err := os.Mkdir(featureDirectory, 0o755); err != nil {
		t.Fatalf("create local Feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(featureDirectory, "devcontainer-feature.json"), []byte(`{"id":"local","version":"1.0.0"}`), 0o600); err != nil {
		t.Fatalf("write local Feature metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(featureDirectory, "install.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write local Feature installer: %v", err)
	}
	resolved, err := (&Engine{}).fetchFeature(context.Background(), "./feature", nil, filepath.Join(t.TempDir(), "resolved"), devcontainer.LockedFeature{}, devcontainer.CacheOptions{}, configurationDirectory, configurationDirectory)
	if err != nil {
		t.Fatalf("load local Feature: %v", err)
	}
	if resolved.metadata.ID != "local" || resolved.lockable {
		t.Fatalf("local Feature = %#v", resolved)
	}
}

func TestLoadFeatureTarball(t *testing.T) {
	t.Parallel()

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for name, content := range map[string]string{
		"devcontainer-feature.json": `{"id":"tarball","version":"1.0.0"}`,
		"install.sh":                "#!/bin/sh\n",
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatalf("write tarball header: %v", err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatalf("write tarball content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tarball: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(archive.Bytes())
	}))
	defer server.Close()
	resolved, err := (&Engine{}).fetchFeature(context.Background(), server.URL+"/feature.tgz", nil, filepath.Join(t.TempDir(), "resolved"), devcontainer.LockedFeature{}, devcontainer.CacheOptions{}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("load tarball Feature: %v", err)
	}
	if resolved.metadata.ID != "tarball" || !resolved.lockable || resolved.digest == "" {
		t.Fatalf("tarball Feature = %#v", resolved)
	}
}

func TestResolveFeatureOptionsAndMergeMetadata(t *testing.T) {
	t.Parallel()

	options, provided, err := resolveFeatureOptions(map[string]featureOption{
		"port":     {Type: "string", Default: json.RawMessage(`"8080"`), Enum: []string{"8080", "13337"}},
		"enabled":  {Type: "boolean", Default: json.RawMessage(`false`)},
		"1_option": {Type: "boolean", Default: json.RawMessage(`false`)},
	}, json.RawMessage(`{"port":"13337","enabled":"true","1_option":true}`))
	if err != nil {
		t.Fatalf("resolve Feature options: %v", err)
	}
	if options["PORT"] != "13337" || options["ENABLED"] != "true" || options["_OPTION"] != "true" || len(provided) != 3 {
		t.Fatalf("Feature options = %#v", options)
	}
	shorthand, _, err := resolveFeatureOptions(map[string]featureOption{"version": {Type: "string"}}, json.RawMessage(`"1.18"`))
	if err != nil || shorthand["VERSION"] != "1.18" {
		t.Fatalf("Feature shorthand = %#v, %v", shorthand, err)
	}

	configuration := devcontainer.Merge(devcontainer.Configuration{}, featureConfigurationFromMetadata(featureMetadata{
		ID:             "editor",
		ContainerEnv:   map[string]string{"PATH_PREFIX": "/tools"},
		Customizations: map[string]json.RawMessage{"vscode": json.RawMessage(`{"extensions":["example.tool"]}`)},
	}))
	if configuration.ContainerEnv["PATH_PREFIX"] != "/tools" || len(configuration.Customizations["vscode"]) == 0 {
		t.Fatalf("merged Feature metadata = %#v", configuration)
	}
	if _, _, err := resolveFeatureOptions(map[string]featureOption{
		"channel": {Type: "string", Enum: []string{"stable"}},
	}, json.RawMessage(`{"channel":"nightly"}`)); err == nil {
		t.Fatal("Feature option outside enum was accepted")
	}
}

func TestNormalizeFeatureRequestSupportsBooleanAndStringForms(t *testing.T) {
	t.Parallel()

	for _, value := range []json.RawMessage{nil, json.RawMessage(`true`), json.RawMessage(`{}`)} {
		enabled, _, err := normalizeFeatureRequest(value)
		if err != nil || !enabled {
			t.Fatalf("normalize Feature request %s: enabled=%v, err=%v", value, enabled, err)
		}
	}
	enabled, _, err := normalizeFeatureRequest(json.RawMessage(`false`))
	if err != nil || enabled {
		t.Fatalf("disabled Feature = %v, %v", enabled, err)
	}
	_, _, err = normalizeFeatureRequest(json.RawMessage(`null`))
	if err == nil {
		t.Fatal("null Feature options were accepted")
	}
	_, _, err = normalizeFeatureRequest(json.RawMessage(`{"version":1}`))
	if err == nil {
		t.Fatal("numeric Feature option was accepted")
	}
}

func TestFindRunArgsUserUsesLastValue(t *testing.T) {
	t.Parallel()

	if user := findRunArgsUser([]string{"--user", "first", "-u=second"}); user != "second" {
		t.Fatalf("runArgs user = %q", user)
	}
}

func TestFeatureLifecycleCommandsPreserveSourceOrder(t *testing.T) {
	t.Parallel()

	var commands devcontainer.LifecycleCommands
	commands.Append(featureConfigurationFromMetadata(featureMetadata{
		ID:                "first",
		PostCreateCommand: devcontainer.Command{Value: json.RawMessage(`{"install":"echo first"}`)},
	}))
	commands.Append(featureConfigurationFromMetadata(featureMetadata{
		ID:                "second",
		PostCreateCommand: devcontainer.Command{Value: json.RawMessage(`{"install":"echo second"}`)},
	}))
	commands.Append(devcontainer.Configuration{PostCreateCommand: devcontainer.Command{Value: json.RawMessage(`{"repository":"echo repository"}`)}})
	if len(commands.PostCreate) != 3 || !bytes.Contains(commands.PostCreate[0].Value, []byte("first")) || !bytes.Contains(commands.PostCreate[2].Value, []byte("repository")) {
		t.Fatalf("lifecycle command groups = %#v", commands.PostCreate)
	}
}

func TestOrderFeaturesUsesDependenciesAndSoftInstallHints(t *testing.T) {
	t.Parallel()

	base := &resolvedFeature{reference: "ghcr.io/example/base:1", references: []string{"ghcr.io/example/base:1"}, metadata: featureMetadata{ID: "base"}}
	tool := &resolvedFeature{reference: "ghcr.io/example/tool:1", references: []string{"ghcr.io/example/tool:1"}, metadata: featureMetadata{ID: "tool"}, hardDependencies: []*resolvedFeature{base}}
	editor := &resolvedFeature{reference: "ghcr.io/example/editor:1", references: []string{"ghcr.io/example/editor:1"}, metadata: featureMetadata{ID: "editor"}, softDependencies: []*resolvedFeature{tool}}
	features := []*resolvedFeature{editor, tool, base}
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

	base.softDependencies = []*resolvedFeature{editor}
	if _, err := orderFeatures(features, nil); err == nil {
		t.Fatal("soft installsAfter cycle was accepted")
	}
}

func TestFeatureEqualityUsesContentAndResolvedOptions(t *testing.T) {
	t.Parallel()

	first := &resolvedFeature{digest: "sha256:content", lockable: true, options: map[string]string{"VERSION": "1"}}
	same := &resolvedFeature{digest: "sha256:content", lockable: true, options: map[string]string{"VERSION": "1"}}
	differentOptions := &resolvedFeature{digest: "sha256:content", lockable: true, options: map[string]string{"VERSION": "2"}}
	local := &resolvedFeature{digest: "sha256:content", options: map[string]string{"VERSION": "1"}}
	if !featuresEqual(first, same) {
		t.Fatal("identical resolved Features were not equal")
	}
	if featuresEqual(first, differentOptions) {
		t.Fatal("Features with different options were equal")
	}
	if featuresEqual(first, local) {
		t.Fatal("local Feature was equal to a fetched Feature")
	}
}
