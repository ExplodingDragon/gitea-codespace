// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"encoding/json"
	"testing"

	"gitea.dev/codespace/internal/devcontainer"
)

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

	configuration := devcontainer.Configuration{}
	mergeFeatureMetadata(&configuration, featureMetadata{
		ID:             "editor",
		RemoteUser:     "developer",
		ContainerEnv:   map[string]string{"PATH_PREFIX": "/tools"},
		Customizations: map[string]json.RawMessage{"vscode": json.RawMessage(`{"extensions":["example.tool"]}`)},
	})
	if configuration.RemoteUser != "developer" || configuration.ContainerEnv["PATH_PREFIX"] != "/tools" || len(configuration.Customizations["vscode"]) == 0 {
		t.Fatalf("merged Feature metadata = %#v", configuration)
	}
}

func TestMergeFeatureLifecycleCommandsFlattensObjects(t *testing.T) {
	t.Parallel()

	configuration := devcontainer.Configuration{
		PostCreateCommand: devcontainer.Command{Value: json.RawMessage(`{"repository":"echo repository"}`)},
	}
	mergeFeatureMetadata(&configuration, featureMetadata{
		ID:                "first",
		PostCreateCommand: devcontainer.Command{Value: json.RawMessage(`{"install":"echo first"}`)},
	})
	mergeFeatureMetadata(&configuration, featureMetadata{
		ID:                "second",
		PostCreateCommand: devcontainer.Command{Value: json.RawMessage(`{"install":"echo second"}`)},
	})

	var commands map[string]json.RawMessage
	if err := json.Unmarshal(configuration.PostCreateCommand.Value, &commands); err != nil {
		t.Fatalf("decode merged lifecycle commands: %v", err)
	}
	want := map[string]string{
		"feature-second-install": `"echo second"`,
		"feature-first-install":  `"echo first"`,
		"repository":             `"echo repository"`,
	}
	if len(commands) != len(want) {
		t.Fatalf("merged lifecycle commands = %s", configuration.PostCreateCommand.Value)
	}
	for name, value := range want {
		if string(commands[name]) != value {
			t.Fatalf("merged lifecycle command %q = %s, want %s", name, commands[name], value)
		}
	}
}
