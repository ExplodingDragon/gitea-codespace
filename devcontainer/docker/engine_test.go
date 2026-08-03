// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"encoding/json"
	"strings"
	"testing"

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
