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
		RemoteEnv:    RemoteEnvironment{"WORKSPACE": stringPointer("${containerWorkspaceFolderBasename}")},
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
	if resolved.ContainerEnv["PATH"] != "/usr/bin:/tools" || resolved.RemoteEnv["WORKSPACE"] == nil || *resolved.RemoteEnv["WORKSPACE"] != "project" {
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
	merged = Merge(Configuration{Customizations: map[string]json.RawMessage{
		"tool": json.RawMessage(`{"large":9007199254740993}`),
	}}, Configuration{Customizations: map[string]json.RawMessage{
		"tool": json.RawMessage(`{"enabled":true}`),
	}})
	if !json.Valid(merged.Customizations["tool"]) || string(merged.Customizations["tool"]) != `{"enabled":true}` {
		t.Fatalf("numeric customization = %s", merged.Customizations["tool"])
	}

	merged = Merge(Configuration{RemoteEnv: RemoteEnvironment{"INHERITED": stringPointer("value")}}, Configuration{
		RemoteEnv: RemoteEnvironment{"INHERITED": nil},
	})
	if value, exists := merged.RemoteEnv["INHERITED"]; !exists || value != nil {
		t.Fatalf("remoteEnv null was not preserved: %#v", merged.RemoteEnv)
	}
}

func TestVariableSubstitutionPreservesShellVariables(t *testing.T) {
	t.Parallel()

	configuration, err := ResolveLocalVariables(Configuration{ContainerEnv: map[string]string{
		"PATH":  "/feature/bin:${PATH}",
		"CACHE": "${env:CACHE_DIR:/cache}",
	}}, "/workspaces/project", nil, "environment-id")
	if err != nil {
		t.Fatalf("resolve local variables: %v", err)
	}
	if configuration.ContainerEnv["PATH"] != "/feature/bin:${PATH}" || configuration.ContainerEnv["CACHE"] != "/cache" {
		t.Fatalf("resolved environment = %#v", configuration.ContainerEnv)
	}
	configuration, err = ResolveContainerVariables(configuration, "/workspaces/project", map[string]string{"PATH": "/usr/bin"})
	if err != nil {
		t.Fatalf("resolve container variables: %v", err)
	}
	if configuration.ContainerEnv["PATH"] != "/feature/bin:${PATH}" {
		t.Fatalf("shell PATH expression = %q", configuration.ContainerEnv["PATH"])
	}
}

func TestHostRequirementsAcceptFractionalCPU(t *testing.T) {
	t.Parallel()

	configuration := Configuration{Image: "ubuntu", HostRequirements: HostRequirements{CPUs: 1.5, Memory: "2gb", Storage: "8gb"}}
	if err := configuration.Validate(); err != nil {
		t.Fatalf("validate host requirements: %v", err)
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
			"3000-3999":          {Label: "wide"},
			"3100-3199":          {Label: "narrow"},
			"3150":               {Label: "exact"},
			`node .+/server\.js`: {Label: "process"},
		},
		OtherPortsAttributes: &PortAttributes{Label: "other"},
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
	if got := PortAttributesForProcess(configuration, 8080, "node /workspace/server.js").Label; got != "process" {
		t.Fatalf("process port attributes = %q", got)
	}
	var zero Port
	if err := json.Unmarshal([]byte(`0`), &zero); err != nil {
		t.Fatalf("decode zero forward port: %v", err)
	}
	if value, err := zero.ContainerPort(); err != nil || value != 0 {
		t.Fatalf("zero forward port = %d, %v", value, err)
	}
}

func stringPointer(value string) *string {
	return &value
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
