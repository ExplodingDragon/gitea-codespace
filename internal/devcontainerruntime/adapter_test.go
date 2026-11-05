// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"encoding/json"
	"testing"

	"gitea.dev/codespace/devcontainer"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestCreateOptionsAddsCodespacePolicy(t *testing.T) {
	t.Parallel()

	request := Request{
		CodespaceUUID: "11111111-2222-4333-8444-555555555555",
		Workspace:     "/workspaces/project",
		Source:        devcontainer.Source{Path: ".devcontainer/devcontainer.json"},
		DevContainer: Configuration{WebIDE: WebIDEConfiguration{
			Enabled: true,
			Options: apiextensionsv1.JSON{Raw: []byte(`{
				"version": "4.121.0",
				"locale": "zh-CN",
				"auth": "password",
				"socket": "/tmp/code-server.sock"
			}`)},
		}},
		Cache: devcontainer.CacheOptions{
			BuildRegistry: "https://registry.example.com/cache",
			BuildScope:    "scope",
		},
		GiteaCustomCA: true,
	}
	options, err := buildCreateOptions(request)
	if err != nil {
		t.Fatalf("create options: %v", err)
	}
	if options.OwnerID != request.CodespaceUUID || options.AllowedPathRoot != request.Workspace {
		t.Fatalf("Codespace identity policy = owner %q, root %q", options.OwnerID, options.AllowedPathRoot)
	}
	if len(options.InjectedFeatures) != 1 || options.InjectedFeatures[0].Reference != webIDEFeatureReference {
		t.Fatal("platform Web IDE Feature is missing")
	}
	if !options.InjectedFeatures[0].InstallOnly {
		t.Fatal("platform Web IDE Feature must leave process startup to the Codespace runtime")
	}
	webIDEOptions := options.InjectedFeatures[0].Options
	if string(webIDEOptions["version"]) != `"4.121.0"` || string(webIDEOptions["locale"]) != `"zh-CN"` {
		t.Fatalf("platform Web IDE administrator options = %#v", webIDEOptions)
	}
	for name, want := range map[string]string{
		"auth": `"none"`, "host": `"0.0.0.0"`, "workspace": `"."`, "socket": `""`, "disableTelemetry": "true", "disableUpdateCheck": "true",
	} {
		if string(webIDEOptions[name]) != want {
			t.Fatalf("platform Web IDE option %s = %s, want %s", name, webIDEOptions[name], want)
		}
	}
	if options.Labels["dev.gitea.codespace.uuid"] != request.CodespaceUUID {
		t.Fatalf("Codespace label = %#v", options.Labels)
	}
	if options.Cache.BuildRegistry != request.Cache.BuildRegistry || options.Cache.BuildScope != request.Cache.BuildScope {
		t.Fatalf("Codespace cache = %#v", options.Cache)
	}
	if options.ContainerEnv["CODESPACE_CA_BUNDLE"] != containerCABundlePath || options.ContainerEnv["NODE_EXTRA_CA_CERTS"] != containerCABundlePath || options.ContainerEnv["GITEA_CA_BUNDLE"] != containerCABundlePath {
		t.Fatalf("custom CA environment = %#v", options.ContainerEnv)
	}
	request.GiteaCustomCA = false
	options, err = buildCreateOptions(request)
	if err != nil {
		t.Fatalf("create options without custom CA: %v", err)
	}
	if len(options.ContainerEnv) != 2 || options.ContainerEnv["CODESPACE_CA_BUNDLE"] != containerCABundlePath || options.ContainerEnv["NODE_EXTRA_CA_CERTS"] != containerCABundlePath {
		t.Fatalf("system CA environment = %#v", options.ContainerEnv)
	}
}

func TestResolveWebIDECustomizations(t *testing.T) {
	t.Parallel()

	repository := json.RawMessage(`{
		"extensions": ["example.base@2.0.0", "example.repository"],
		"settings": {
			"editor.tabSize": 2,
			"files.autoSave": "afterDelay"
		}
	}`)

	resolved, err := resolveWebIDECustomizations(repository)
	require.NoError(t, err)
	require.Equal(t, []string{"example.base@2.0.0", "example.repository"}, resolved.Extensions)
	require.JSONEq(t, `2`, string(resolved.Settings["editor.tabSize"]))
	require.JSONEq(t, `"afterDelay"`, string(resolved.Settings["files.autoSave"]))
}
