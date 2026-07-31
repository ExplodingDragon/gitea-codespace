// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"testing"

	"gitea.dev/codespace/devcontainer"
)

func TestCreateOptionsAddsCodespacePolicy(t *testing.T) {
	t.Parallel()

	request := Request{
		CodespaceUUID:     "11111111-2222-4333-8444-555555555555",
		Workspace:         "/workspaces/project",
		Source:            devcontainer.Source{Path: ".devcontainer/devcontainer.json"},
		CodeServerVersion: "4.121.0",
		Cache: devcontainer.CacheOptions{
			BuildRegistry: "https://registry.example.com/cache",
			BuildScope:    "scope",
		},
	}
	options, err := BuildCreateOptions(request)
	if err != nil {
		t.Fatalf("create options: %v", err)
	}
	if options.OwnerID != request.CodespaceUUID || options.AllowedPathRoot != request.Workspace {
		t.Fatalf("Codespace identity policy = owner %q, root %q", options.OwnerID, options.AllowedPathRoot)
	}
	if len(options.InjectedFeatures) != 1 || options.InjectedFeatures[0].Reference != codeServerFeatureReference {
		t.Fatal("platform Web IDE Feature is missing")
	}
	if !options.InjectedFeatures[0].InstallOnly {
		t.Fatal("platform Web IDE Feature must leave process startup to the Codespace runtime")
	}
	if string(options.InjectedFeatures[0].Options["version"]) != `"4.121.0"` {
		t.Fatalf("platform Web IDE version = %s", options.InjectedFeatures[0].Options["version"])
	}
	if options.Labels["dev.gitea.codespace.uuid"] != request.CodespaceUUID {
		t.Fatalf("Codespace label = %#v", options.Labels)
	}
	if options.Cache.BuildRegistry != request.Cache.BuildRegistry || options.Cache.BuildScope != request.Cache.BuildScope {
		t.Fatalf("Codespace cache = %#v", options.Cache)
	}
}
