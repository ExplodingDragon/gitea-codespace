// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"encoding/json"
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
	if err := checkHostRequirements(devcontainer.HostRequirements{GPU: json.RawMessage(`"optional"`)}, t.TempDir()); err != nil {
		t.Fatalf("optional GPU requirement: %v", err)
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
