// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeAndContainerVariables(t *testing.T) {
	t.Parallel()

	local, err := ResolveLocalVariables(Configuration{
		Mounts: []Mount{{Type: "volume", Source: "${localEnv:FEATURE_OWNER:owner}", Target: "/feature"}},
	}, "/workspaces/project", nil, "environment-id")
	if err != nil {
		t.Fatalf("resolve local variables: %v", err)
	}
	if local.Mounts[0].Source != "owner" {
		t.Fatalf("resolved local Feature mount = %#v", local.Mounts[0])
	}

	base := Configuration{
		ContainerEnv: map[string]string{"PATH": "/usr/bin", "BASE": "base"},
		Mounts:       []Mount{{Type: "volume", Source: "base", Target: "/data"}},
		Customizations: map[string]json.RawMessage{
			"vscode": json.RawMessage(`{"extensions":["one"],"settings":{"editor.tabSize":2}}`),
		},
	}
	override := Configuration{
		ContainerEnv: map[string]string{"PATH": "${containerEnv:PATH}:/tools"},
		RemoteEnv:    map[string]string{"WORKSPACE": "${containerWorkspaceFolderBasename}"},
		Mounts:       []Mount{{Type: "volume", Source: "override", Target: "/data"}},
		Customizations: map[string]json.RawMessage{
			"vscode": json.RawMessage(`{"extensions":["one","two"],"settings":{"editor.wordWrap":"on"}}`),
		},
	}
	merged := Merge(base, override)
	resolved, err := ResolveContainerVariables(merged, "/workspaces/project", map[string]string{"PATH": "/usr/bin"})
	if err != nil {
		t.Fatalf("resolve container variables: %v", err)
	}
	if resolved.ContainerEnv["PATH"] != "/usr/bin:/tools" || resolved.RemoteEnv["WORKSPACE"] != "project" {
		t.Fatalf("resolved environment = %#v / %#v", resolved.ContainerEnv, resolved.RemoteEnv)
	}
	if len(resolved.Mounts) != 1 || resolved.Mounts[0].Source != "override" {
		t.Fatalf("merged mounts = %#v", resolved.Mounts)
	}
	var vscode struct {
		Extensions []string       `json:"extensions"`
		Settings   map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(resolved.Customizations["vscode"], &vscode); err != nil {
		t.Fatalf("decode customizations: %v", err)
	}
	if len(vscode.Extensions) != 2 || vscode.Settings["editor.wordWrap"] != "on" {
		t.Fatalf("merged customizations = %#v", vscode)
	}
}

func TestMountAndPortAttributes(t *testing.T) {
	t.Parallel()

	var value Mount
	if err := json.Unmarshal([]byte(`"type=bind,source=/host,target=/data,readonly,bind-propagation=rshared"`), &value); err != nil {
		t.Fatalf("decode mount: %v", err)
	}
	if !value.ReadOnly || value.BindPropagation != "rshared" {
		t.Fatalf("mount = %#v", value)
	}
	configuration := Configuration{
		PortsAttributes: map[string]PortAttributes{
			"3000-3999": {Label: "wide"},
			"3100-3199": {Label: "narrow"},
			"3150":      {Label: "exact"},
		},
		OtherPortsAttributes: PortAttributes{Label: "other"},
	}
	if got := PortAttributesFor(configuration, 3150).Label; got != "exact" {
		t.Fatalf("exact port attributes = %q", got)
	}
	if got := PortAttributesFor(configuration, 3120).Label; got != "narrow" {
		t.Fatalf("range port attributes = %q", got)
	}
	if got := PortAttributesFor(configuration, 8080).Label; got != "other" {
		t.Fatalf("fallback port attributes = %q", got)
	}
}

func TestLockfileRoundTripAndFrozenMode(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	configurationPath := filepath.Join(directory, "devcontainer.json")
	lockfile := Lockfile{Features: map[string]LockedFeature{
		"ghcr.io/example/feature:1": {Version: "1.2.3", Resolved: "ghcr.io/example/feature@sha256:abc", Integrity: "sha256:abc"},
	}}
	if err := WriteLockfile(configurationPath, lockfile, false); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
	loaded, exists, err := ReadLockfile(configurationPath)
	if err != nil || !exists || loaded.Features["ghcr.io/example/feature:1"].Version != "1.2.3" {
		t.Fatalf("read lockfile = %#v, exists=%v, err=%v", loaded, exists, err)
	}
	if err := WriteLockfile(configurationPath, lockfile, true); err != nil {
		t.Fatalf("validate frozen lockfile: %v", err)
	}
	if err := os.Remove(LockfilePath(configurationPath)); err != nil {
		t.Fatalf("remove lockfile: %v", err)
	}
	if err := WriteLockfile(configurationPath, lockfile, true); err == nil {
		t.Fatal("frozen mode accepted a missing lockfile")
	}
}
