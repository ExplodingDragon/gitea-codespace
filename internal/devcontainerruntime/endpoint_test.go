// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"strings"
	"testing"

	"gitea.dev/codespace/devcontainer"
)

func TestDevContainerPortAcceptsLoopbackForwardPort(t *testing.T) {
	t.Parallel()

	service, forwarded, err := devContainerPort(devcontainer.Port{Address: "[::1]:9090"})
	if err != nil {
		t.Fatalf("resolve forwarded port: %v", err)
	}
	if service != "" || forwarded != 9090 {
		t.Fatalf("forwardPorts = %q:%d", service, forwarded)
	}
}

func TestConfiguredEndpointsResolveComposeService(t *testing.T) {
	t.Parallel()

	state := &devcontainer.State{
		Version: devcontainer.StateFormatVersion, ID: "environment", OwnerID: "owner",
		ConfigurationPath: "/workspace/.devcontainer/devcontainer.json", ConfigurationSHA256: strings.Repeat("a", 64),
		Workspace: "/workspace", WorkspaceFolder: "/", RemoteWorkdir: "/", RemoteUser: "vscode",
		ComposeProject: "project", PrimaryService: "app", PrimaryContainerID: "app-container",
		RelatedContainerIDs: []string{"db-container"}, ComposeContainers: map[string]string{"app": "app-container", "db": "db-container"},
		Configuration: devcontainer.Configuration{
			ForwardPorts:    []devcontainer.Port{{Address: "db:5432"}},
			PortsAttributes: map[string]devcontainer.PortAttributes{"db:5432": {Label: "Database"}},
		},
	}
	endpoints, targets, err := ConfiguredEndpoints(state)
	if err != nil {
		t.Fatalf("resolve configured endpoints: %v", err)
	}
	if len(endpoints) != 1 || endpoints[0].Port != 5432 || endpoints[0].Label != "Database" || targets[endpoints[0].EndpointId] != "db-container" {
		t.Fatalf("configured endpoints = %#v, targets = %#v", endpoints, targets)
	}

	state.Configuration.ForwardPorts[0].Address = "cache:6379"
	if _, _, err := ConfiguredEndpoints(state); err == nil {
		t.Fatal("missing Compose service was accepted")
	}
}
