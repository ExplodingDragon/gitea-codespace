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
			Source: provisioner.DevContainerSourceRepository,
			Path:   ".devcontainer/devcontainer.json",
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

func TestRegistryCacheRejectsUnauthorizedRepositories(t *testing.T) {
	t.Parallel()

	cache := &registryCache{
		secret: []byte("secret"),
		upstreams: map[string]registryCacheUpstream{
			"ghcr.io": {allow: []string{"devcontainers/*"}},
		},
	}
	repoHash := registryCacheRepoHash("owner/repo")
	request, err := http.NewRequest(http.MethodGet, "/v2/cache/"+repoHash+"/build/manifests/cache", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(registryCacheUsername, cache.issueToken(repoHash, time.Now().Add(time.Minute)))

	for _, tc := range []struct {
		name       string
		repository string
		action     string
	}{
		{name: "other cache namespace", repository: "cache/" + registryCacheRepoHash("other/repo") + "/build", action: "pull"},
		{name: "unknown mirror host", repository: "mirror/docker.io/library/ubuntu", action: "pull"},
		{name: "disallowed mirror path", repository: "mirror/ghcr.io/other/image", action: "pull"},
		{name: "unsupported action", repository: "cache/" + repoHash + "/build", action: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := cache.authorize(request, tc.repository, tc.action); err == nil {
				t.Fatalf("authorized %s %s", tc.action, tc.repository)
			}
		})
	}
}

func TestRegistryCacheRejectsExpiredToken(t *testing.T) {
	t.Parallel()

	cache := &registryCache{secret: []byte("secret")}
	repoHash := registryCacheRepoHash("owner/repo")
	request, err := http.NewRequest(http.MethodGet, "/v2/cache/"+repoHash+"/build/manifests/cache", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(registryCacheUsername, cache.issueToken(repoHash, time.Now().Add(-time.Minute)))

	if err := cache.authorize(request, "cache/"+repoHash+"/build", "pull"); err == nil {
		t.Fatal("authorized expired cache token")
	}
}

func TestRegistryCacheBlobMountRequiresSameRepository(t *testing.T) {
	t.Parallel()

	cache := &registryCache{
		secret: []byte("secret"),
	}
	repoHash := registryCacheRepoHash("owner/repo")
	repository := "cache/" + repoHash + "/build"
	request, err := http.NewRequest(http.MethodPost, "/v2/"+repository+"/blobs/uploads/?mount=sha256:abc&from="+repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(registryCacheUsername, cache.issueToken(repoHash, time.Now().Add(time.Minute)))
	if err := cache.authorize(request, repository, "push"); err != nil {
		t.Fatalf("authorize same-repository blob mount: %v", err)
	}

	request, err = http.NewRequest(http.MethodPost, "/v2/"+repository+"/blobs/uploads/?mount=sha256:abc&from=cache/other/build", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(registryCacheUsername, cache.issueToken(repoHash, time.Now().Add(time.Minute)))
	if err := cache.authorize(request, repository, "push"); err == nil {
		t.Fatal("authorized cross-repository blob mount")
	}
}
