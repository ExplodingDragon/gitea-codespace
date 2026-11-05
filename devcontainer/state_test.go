// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStateValidatesCompleteComposeServiceMapping(t *testing.T) {
	t.Parallel()

	state := &State{
		Version: StateFormatVersion, ID: "environment", OwnerID: "owner",
		ConfigurationPath: "/workspace/.devcontainer/devcontainer.json", ConfigurationSHA256: strings.Repeat("a", 64),
		Workspace: "/workspace", WorkspaceFolder: "/", RemoteWorkdir: "/", RemoteUser: "vscode",
		ComposeProject: "project", PrimaryService: "app", PrimaryContainerID: "app-container",
		RelatedContainerIDs: []string{"db-container"}, ComposeContainers: map[string]string{"app": "app-container", "db": "db-container"},
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("validate Compose state: %v", err)
	}

	state.ComposeContainers = map[string]string{"app": "app-container"}
	if err := state.Validate(); err == nil {
		t.Fatal("incomplete Compose service mapping was accepted")
	}
	state.ComposeContainers = map[string]string{"app": "app-container", "db": "app-container"}
	if err := state.Validate(); err == nil {
		t.Fatal("ambiguous Compose service mapping was accepted")
	}
}

func TestStateUsesGroupedLifecycleCommands(t *testing.T) {
	t.Parallel()

	state := &State{
		Version: StateFormatVersion, ID: "environment", OwnerID: "owner",
		ConfigurationPath: "/workspace/.devcontainer/devcontainer.json", ConfigurationSHA256: strings.Repeat("a", 64),
		Workspace: "/workspace", WorkspaceFolder: "/workspace", RemoteWorkdir: "/workspace", RemoteUser: "vscode",
		PrimaryContainerID: "container",
		Configuration:      Configuration{InitializeCommand: Command{Value: json.RawMessage(`"prepare"`)}},
		LifecycleCommands: LifecycleCommands{OnCreate: []Command{
			{Value: json.RawMessage(`"image command"`)},
			{Value: json.RawMessage(`"repository command"`)},
		}},
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("validate state: %v", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	decoded := &State{}
	if err := json.Unmarshal(data, decoded); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("validate decoded state: %v", err)
	}
	if len(decoded.LifecycleCommands.OnCreate) != 2 || string(decoded.Configuration.InitializeCommand.Value) != `"prepare"` {
		t.Fatalf("decoded lifecycle state = %#v", decoded)
	}

	decoded.Configuration.PostCreateCommand = Command{Value: json.RawMessage(`"duplicate"`)}
	if err := decoded.Validate(); err == nil {
		t.Fatal("flattened lifecycle command was accepted in recovery state")
	}
}
