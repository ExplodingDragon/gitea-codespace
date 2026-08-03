// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gitea.dev/codespace/internal/provisioner"
)

func TestRegistryCacheOptionsUseRepositoryScopedNamespace(t *testing.T) {
	t.Parallel()

	cache := &registryCache{
		enabled:           true,
		publicURL:         "http://10.0.3.1:15000",
		host:              "10.0.3.1:15000",
		secret:            []byte("secret"),
		codeServerVersion: "4.121.0",
		upstreams: map[string]registryCacheUpstream{
			"ghcr.io": {},
		},
	}
	options := cache.CacheOptions(provisioner.LifecycleRequest{
		RepoFullName:   "Owner/Repo",
		EnvironmentTag: "default",
		DevContainer: provisioner.DevContainerConfiguration{
			Source:        provisioner.DevContainerSourceRepository,
			Path:          ".devcontainer/devcontainer.json",
			ContentSHA256: strings.Repeat("a", 64),
		},
	})
	if !strings.HasPrefix(options.BuildRegistry, "http://10.0.3.1:15000/cache/") || strings.Contains(options.BuildRegistry, "Owner") {
		t.Fatalf("build registry namespace = %q", options.BuildRegistry)
	}
	if options.Mirrors["ghcr.io"] != "http://10.0.3.1:15000/mirror/ghcr.io" {
		t.Fatalf("mirror = %#v", options.Mirrors)
	}
	if _, ok := options.Credentials["10.0.3.1:15000"]; !ok {
		t.Fatalf("registry credentials = %#v", options.Credentials)
	}
}

func TestRegistryCacheAuthorizesScopedCacheRepository(t *testing.T) {
	t.Parallel()

	cache := &registryCache{
		secret: []byte("secret"),
		upstreams: map[string]registryCacheUpstream{
			"ghcr.io": {allow: []string{"devcontainers/*"}},
		},
	}
	repoHash := registryCacheRepoHash("owner/repo")
	request, err := http.NewRequest(http.MethodPut, "/v2/cache/"+repoHash+"/build/manifests/cache", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(registryCacheUsername, cache.issueToken(repoHash, time.Now().Add(time.Minute)))
	if err := cache.authorize(request, "cache/"+repoHash+"/build", "push"); err != nil {
		t.Fatalf("authorize cache push: %v", err)
	}
	if err := cache.authorize(request, "mirror/ghcr.io/devcontainers/features/go", "pull"); err != nil {
		t.Fatalf("authorize mirror pull: %v", err)
	}
}
