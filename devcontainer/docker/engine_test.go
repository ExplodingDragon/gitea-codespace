// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"

	"gitea.dev/codespace/devcontainer"
)

func TestEngineConfigurationHelpers(t *testing.T) {
	t.Parallel()

	mounts, err := devContainerMounts("/host/workspace", "/workspaces/project", "", []devcontainer.Mount{
		{Type: "volume", Source: "replacement", Target: "/workspaces/project", VolumeNoCopy: true},
		{Type: "bind", Source: "/host/cache", Target: "/cache", BindPropagation: "rshared"},
	})
	if err != nil {
		t.Fatalf("resolve mounts: %v", err)
	}
	if len(mounts) != 2 || mounts[0].Source != "replacement" || !mounts[0].VolumeNoCopy || mounts[1].BindPropagation != "rshared" {
		t.Fatalf("resolved mounts = %#v", mounts)
	}
	if composeProjectName("owner-a") == composeProjectName("ownera") {
		t.Fatal("distinct owner IDs produced the same Compose project name")
	}
	publishSpecs, err := appPortPublishSpecs(devcontainer.AppPortList{
		{Number: 8000, Numeric: true},
		{Address: "127.0.0.1:9000:90"},
	})
	if err != nil {
		t.Fatalf("resolve appPort publish specs: %v", err)
	}
	if !slices.Equal(publishSpecs, []string{"127.0.0.1:8000:8000", "127.0.0.1:9000:90"}) {
		t.Fatalf("appPort publish specs = %#v", publishSpecs)
	}
}

func TestDockerCreateArgumentsWithRunArgsPreserveFeatureStartup(t *testing.T) {
	t.Parallel()

	resolved := &devcontainer.ResolvedConfiguration{
		DevContainerID: "environment-1",
		Configuration:  devcontainer.Configuration{RunArgs: []string{"--hostname", "workspace"}},
	}
	config := &container.Config{
		Image:        "example/image:latest",
		WorkingDir:   "/workspace",
		User:         "codespace",
		Env:          []string{"A=B"},
		Labels:       map[string]string{"z": "last", "a": "first"},
		Entrypoint:   []string{"/bin/sh"},
		Cmd:          []string{"-c", "feature-entrypoint\nexec \"$@\"", "-", "/start.sh"},
		ExposedPorts: nil,
	}
	hostConfig := &container.HostConfig{
		Mounts:     []mount.Mount{{Type: mount.TypeBind, Source: "/host/workspace", Target: "/workspace"}},
		Privileged: true,
	}
	arguments, err := dockerCreateArguments(resolved, config, hostConfig, []string{"127.0.0.1:8000:8000"})
	if err != nil {
		t.Fatalf("create Docker arguments: %v", err)
	}
	for _, required := range [][]string{
		{"--publish", "127.0.0.1:8000:8000"},
		{"--hostname", "workspace"},
		{"--entrypoint", "/bin/sh"},
		{"example/image:latest", "-c"},
	} {
		if !containsAdjacent(arguments, required[0], required[1]) {
			t.Fatalf("Docker arguments missing %q %q: %#v", required[0], required[1], arguments)
		}
	}
	imageIndex := slices.Index(arguments, "example/image:latest")
	if imageIndex < 0 || imageIndex+4 >= len(arguments) || arguments[imageIndex+1] != "-c" || arguments[imageIndex+3] != "-" || arguments[imageIndex+4] != "/start.sh" {
		t.Fatalf("Docker command does not preserve startup wrapper after image: %#v", arguments)
	}
}

func TestDockerCreateArgumentsRejectEntrypointRunArg(t *testing.T) {
	t.Parallel()

	_, err := dockerCreateArguments(&devcontainer.ResolvedConfiguration{
		Configuration: devcontainer.Configuration{RunArgs: []string{"--entrypoint=/custom"}},
	}, &container.Config{Image: "example/image"}, &container.HostConfig{}, nil)
	if err == nil || !strings.Contains(err.Error(), "Feature startup entrypoints") {
		t.Fatalf("entrypoint runArg error = %v", err)
	}
}

func containsAdjacent(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}

func TestCacheReferences(t *testing.T) {
	t.Parallel()

	mirrors := map[string]string{
		"docker.io": "https://cache.example.com/docker",
		"ghcr.io":   "http://cache.example.com/ghcr",
	}
	image, mirrored, err := mirroredReference("ubuntu:24.04", mirrors)
	if err != nil || !mirrored || image != "cache.example.com/docker/library/ubuntu:24.04" {
		t.Fatalf("Docker Hub mirror = %q, %v, %v", image, mirrored, err)
	}
	digest := strings.Repeat("a", 64)
	image, mirrored, err = mirroredReference("ghcr.io/example/tool@sha256:"+digest, mirrors)
	if err != nil || !mirrored || image != "cache.example.com/ghcr/example/tool@sha256:"+digest {
		t.Fatalf("GHCR mirror = %q, %v, %v", image, mirrored, err)
	}
	cache := devcontainer.CacheOptions{BuildRegistry: "https://registry.example.com/codespace", BuildScope: "scope"}
	first := buildCacheReference(cache, "features")
	if first == "" || first == buildCacheReference(cache, "remote-user") || first != buildCacheReference(cache, "features") {
		t.Fatalf("BuildKit cache references are not stable and stage-specific: %q", first)
	}
	featureImage := featureImageArtifactCacheReference(cache, "sha256:base", []*resolvedFeature{{
		reference: "ghcr.io/devcontainers/features/git:1",
		digest:    "sha256:feature",
		options:   map[string]string{"version": "latest"},
	}}, "root", "root", map[string]string{"PATH": "/usr/bin"})
	if featureImage == "" || featureImage == first {
		t.Fatalf("Feature image cache reference = %q", featureImage)
	}
	changedFeature := featureImageArtifactCacheReference(cache, "sha256:base", []*resolvedFeature{{
		reference: "ghcr.io/devcontainers/features/git:1",
		digest:    "sha256:changed",
		options:   map[string]string{"version": "latest"},
	}}, "root", "root", map[string]string{"PATH": "/usr/bin"})
	if featureImage == changedFeature || featureImage != featureImageArtifactCacheReference(cache, "sha256:base", []*resolvedFeature{{
		reference: "ghcr.io/devcontainers/features/git:1",
		digest:    "sha256:feature",
		options:   map[string]string{"version": "latest"},
	}}, "root", "root", map[string]string{"PATH": "/usr/bin"}) {
		t.Fatalf("Feature image cache reference is not stable and content-specific: %q / %q", featureImage, changedFeature)
	}
}

func TestMergeInjectedFeaturesRejectsConflicts(t *testing.T) {
	t.Parallel()

	resolved := &devcontainer.ResolvedConfiguration{Configuration: devcontainer.Configuration{Features: map[string]json.RawMessage{
		"ghcr.io/example/features/tool:1": json.RawMessage(`{"version":"1"}`),
	}}}
	if err := mergeInjectedFeatures(resolved, []devcontainer.InjectedFeature{{
		Reference: "ghcr.io/example/features/tool:1",
		Origin:    "user",
		Options:   map[string]json.RawMessage{"version": json.RawMessage(`"2"`)},
	}}); err == nil {
		t.Fatal("expected repository and user Feature conflict")
	}

	resolved = &devcontainer.ResolvedConfiguration{Configuration: devcontainer.Configuration{Features: map[string]json.RawMessage{
		"ghcr.io/example/features/tool:1": json.RawMessage(`{"version":"1"}`),
	}}}
	if err := mergeInjectedFeatures(resolved, []devcontainer.InjectedFeature{{
		Reference: "ghcr.io/example/features/tool:1",
		Origin:    "user",
		Options:   map[string]json.RawMessage{"version": json.RawMessage(`"1"`)},
	}}); err != nil {
		t.Fatalf("deduplicate matching Feature: %v", err)
	}
	if len(resolved.InjectedFeatureReferences) != 0 {
		t.Fatalf("repository-owned matching Feature marked as injected: %#v", resolved.InjectedFeatureReferences)
	}

	resolved = &devcontainer.ResolvedConfiguration{Configuration: devcontainer.Configuration{Features: map[string]json.RawMessage{
		"ghcr.io/example/features/tool:1": json.RawMessage(`{}`),
	}}}
	if err := mergeInjectedFeatures(resolved, []devcontainer.InjectedFeature{{
		Reference: "ghcr.io/example/features/tool:2",
		Origin:    "platform",
	}}); err == nil {
		t.Fatal("expected conflicting versions of the same Feature to fail")
	}
}

func TestMergeInjectedFeaturesRecordsInstallOnlyPolicy(t *testing.T) {
	t.Parallel()

	const reference = "ghcr.io/example/features/editor:1"
	resolved := &devcontainer.ResolvedConfiguration{Configuration: devcontainer.Configuration{Features: map[string]json.RawMessage{
		reference: json.RawMessage(`{"version":"1"}`),
	}}}
	if err := mergeInjectedFeatures(resolved, []devcontainer.InjectedFeature{{
		Reference:   reference,
		Origin:      "platform",
		Options:     map[string]json.RawMessage{"version": json.RawMessage(`"1"`)},
		InstallOnly: true,
	}}); err != nil {
		t.Fatalf("merge matching install-only Feature: %v", err)
	}
	if _, ok := resolved.InstallOnlyFeatures[reference]; !ok {
		t.Fatalf("install-only Feature policy = %#v", resolved.InstallOnlyFeatures)
	}
}
