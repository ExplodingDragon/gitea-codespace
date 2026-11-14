// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/buildkit/frontend/dockerfile/parser"

	"gitea.dev/codespace/devcontainer"
)

func TestFeatureBuildContext(t *testing.T) {
	t.Parallel()

	featureDirectory := t.TempDir()
	installer := []byte("#!/bin/sh\nprintf '%s\\n' \"$MESSAGE\"\n")
	if err := os.WriteFile(filepath.Join(featureDirectory, "install.sh"), installer, 0o600); err != nil {
		t.Fatal(err)
	}
	features := []*resolvedFeature{
		{directory: featureDirectory, options: map[string]string{"MESSAGE": "it's a message"}},
		{directory: featureDirectory, options: map[string]string{"MESSAGE": "second"}},
	}
	directory := t.TempDir()
	if err := writeFeatureBuildContext(directory, "example:base", features, "developer", "remote", map[string]string{"EXAMPLE": "value"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "Dockerfile.features"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.Parse(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	steps := parsed.AST.Children
	if len(steps) != 7 || !strings.EqualFold(steps[0].Value, "FROM") || steps[0].Next.Value != "example:base" {
		t.Fatalf("Feature build instructions = %s", parsed.AST.Dump())
	}
	if !strings.EqualFold(steps[1].Value, "USER") || steps[1].Next.Value != "root" {
		t.Fatalf("Feature installation user = %s", steps[1].Dump())
	}
	for index, feature := range features {
		run := steps[index+4]
		if !strings.EqualFold(run.Value, "RUN") || len(run.Flags) != 1 || run.Flags[0] != fmt.Sprintf("--mount=type=bind,source=feature/%d,target=/tmp/dev-container-feature-source", index) {
			t.Fatalf("Feature instruction = %s", run.Dump())
		}
		command := run.Next.Value
		for _, step := range []string{
			fmt.Sprintf("cp -R /tmp/dev-container-feature-source /tmp/dev-container-feature/%d", index),
			fmt.Sprintf("cd /tmp/dev-container-feature/%d && chmod +x ./install.sh", index),
			". ../devcontainer-features.builtin.env && . ./devcontainer-features.env",
			fmt.Sprintf("./install.sh && rm -rf /tmp/dev-container-feature/%d", index),
		} {
			if !strings.Contains(command, step) {
				t.Fatalf("Feature RUN missing %q: %s", step, command)
			}
		}
		environment, err := os.ReadFile(filepath.Join(directory, "feature", fmt.Sprint(index), "devcontainer-features.env"))
		if err != nil || string(environment) != "MESSAGE="+shellQuote(feature.options["MESSAGE"])+"\n" {
			t.Fatalf("Feature environment = %q, %v", environment, err)
		}
		copiedInstaller, err := os.ReadFile(filepath.Join(directory, "feature", fmt.Sprint(index), "install.sh"))
		if err != nil || !bytes.Equal(copiedInstaller, installer) {
			t.Fatalf("Feature installer = %q, %v", copiedInstaller, err)
		}
	}
	if !strings.EqualFold(steps[6].Value, "USER") || steps[6].Next.Value != "developer" {
		t.Fatalf("final container user = %s", steps[6].Dump())
	}
}

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

func TestResolveFeaturesExpandsDependenciesAndDeduplicatesContent(t *testing.T) {
	t.Parallel()

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for name, content := range map[string]string{
		"devcontainer-feature.json": `{"id":"base","version":"1.0.0","options":{"version":{"type":"string","default":"1"}}}`,
		"install.sh":                "#!/bin/sh\n",
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(archive.Bytes())
	}))
	defer server.Close()
	baseReference, aliasReference := server.URL+"/base.tgz", server.URL+"/alias.tgz"
	variantReference := server.URL + "/variant.tgz"
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(featureMetadata{ID: "tool", Version: "1.0.0", DependsOn: map[string]json.RawMessage{baseReference: json.RawMessage(`{"version":"1"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"devcontainer-feature.json": metadata, "install.sh": []byte("#!/bin/sh\n")} {
		if err := os.WriteFile(filepath.Join(directory, "tool", name), content, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	resolved := &devcontainer.ResolvedConfiguration{
		ConfigurationPath: filepath.Join(directory, "devcontainer.json"), ConfigurationDir: directory, AllowedPathRoot: directory,
		Configuration: devcontainer.Configuration{Features: map[string]json.RawMessage{
			"./tool": json.RawMessage(`true`), "./disabled": json.RawMessage(`false`),
			baseReference: json.RawMessage(`{"version":"2"}`), aliasReference: json.RawMessage(`{"version":"2"}`),
			variantReference: json.RawMessage(`{"version":"3"}`),
		}},
	}
	features, err := (&Engine{}).resolveFeatures(t.Context(), resolved, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(features) != 3 || features[0].metadata.ID != "base" || features[1].metadata.ID != "base" || features[2].metadata.ID != "tool" {
		t.Fatalf("resolved Features = %#v", features)
	}
	base, variant, tool := features[0], features[1], features[2]
	if !base.lockable || base.digest == "" || base.options["VERSION"] != "2" || len(base.references) != 2 || len(tool.hardDependencies) != 1 || tool.hardDependencies[0] != base {
		t.Fatalf("resolved dependency = %#v, tool dependencies = %#v", base, tool.hardDependencies)
	}
	if variant.digest != base.digest || variant.options["VERSION"] != "3" || len(variant.references) != 1 {
		t.Fatalf("Feature with distinct options = %#v", variant)
	}
	if err := writeResolvedFeatureLockfile(resolved, features); err != nil {
		t.Fatal(err)
	}
	lockfile, exists, err := devcontainer.ReadLockfile(resolved.ConfigurationPath)
	if err != nil || !exists || len(lockfile.Features) != 3 || lockfile.Features[baseReference].Integrity != base.digest || lockfile.Features[aliasReference].Integrity != base.digest || lockfile.Features[variantReference].Integrity != variant.digest {
		t.Fatalf("resolved lockfile = %#v, exists = %v, error = %v", lockfile, exists, err)
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
