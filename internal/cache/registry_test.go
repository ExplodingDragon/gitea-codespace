// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/manifest/schema2"
	"github.com/distribution/distribution/v3/registry/storage"
	"github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	"github.com/stretchr/testify/require"
)

func attachTestCacheControl(t *testing.T, cache *registryCache) {
	t.Helper()
	cache.tokenKey = []byte(strings.Repeat("t", 32))
}

func testMirrorRepository(host, image string) string {
	return "mirror/" + configpkg.CacheUpstreamID(host) + "/" + image
}

func testRegistryToken(t *testing.T, cache *registryCache, namespace string, policy configpkg.CachePolicy) string {
	t.Helper()
	if cache.id == "" {
		cache.id = "test"
	}
	if len(cache.tokenKey) == 0 {
		attachTestCacheControl(t, cache)
	}
	token, err := SignCredential(cache.tokenKey, cache.id, namespace, policy, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRegistryCacheProxyRefreshesTagsAndRejectsWrites(t *testing.T) {
	var revision atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("upstream received instance cache credentials")
		}
		if r.URL.Path == "/v2/" {
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/v2/team/image/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		content := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":2,"digest":"` + digest.FromString("config").String() + `"},"layers":[]}`
		if revision.Load() != 0 {
			content = strings.Replace(content, `"size":2`, `"size":3`, 1)
		}
		w.Header().Set("Content-Type", schema2.MediaTypeManifest)
		w.Header().Set("Docker-Content-Digest", digest.FromString(content).String())
		_, _ = w.Write([]byte(content))
	}))
	defer upstream.Close()
	transport := http.DefaultTransport
	http.DefaultTransport = upstream.Client().Transport
	defer func() { http.DefaultTransport = transport }()
	host := strings.TrimPrefix(upstream.URL, "https://")
	config := configpkg.CacheConfig{ID: "test", Name: "Test",
		Enabled: true, Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: t.TempDir()},
		MaxAge: configpkg.Duration(time.Hour), Upstreams: map[string]configpkg.RuntimeCacheUpstreamConfig{host: {Allow: []string{"team/*"}}},
	}
	cache, err := newRegistryCache(t.Context(), config, "test-secret", []byte(strings.Repeat("t", 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { require.NoError(t, cache.Close()) }()
	ping := httptest.NewRecorder()
	cache.httpHandler().ServeHTTP(ping, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if ping.Code != http.StatusUnauthorized || ping.Header().Get("WWW-Authenticate") != registryAuthenticationChallenge {
		t.Fatalf("registry discovery did not advertise authentication: %d", ping.Code)
	}
	unknown := httptest.NewRecorder()
	cache.httpHandler().ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusUnauthorized, unknown.Code)
	path := "/v2/" + testMirrorRepository(host, "team/image/manifests/latest")
	var previous string
	for i := range 2 {
		revision.Store(int32(i))
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.SetBasicAuth(Username, testRegistryToken(t, cache, "repository", configpkg.CachePolicy{Public: true, Build: true}))
		response := httptest.NewRecorder()
		cache.httpHandler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("proxy response: %d %s", response.Code, response.Body.String())
		}
		current := response.Header().Get("Docker-Content-Digest")
		if current == "" || current == previous {
			t.Fatalf("tag digest did not refresh: %q", current)
		}
		previous = current
	}
	request := httptest.NewRequest(http.MethodPut, path, strings.NewReader("untrusted image"))
	request.SetBasicAuth(Username, testRegistryToken(t, cache, "repository", configpkg.CachePolicy{Public: true, Build: true}))
	response := httptest.NewRecorder()
	cache.httpHandler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("public cache write status: %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, path, nil)
	request.SetBasicAuth(Username, testRegistryToken(t, cache, "repository", configpkg.CachePolicy{Build: true}))
	response = httptest.NewRecorder()
	cache.httpHandler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("disabled public cache policy: %d", response.Code)
	}
	cache.minFreeBytes = 1 << 62
	request.SetBasicAuth(Username, testRegistryToken(t, cache, "repository", configpkg.CachePolicy{Public: true}))
	response = httptest.NewRecorder()
	cache.httpHandler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cache low-space status: %d", response.Code)
	}
}

func TestRegistryCacheServesScopedBuildBlobs(t *testing.T) {
	config := configpkg.CacheConfig{ID: "test", Name: "Test", Enabled: true, Listen: "127.0.0.1:0",
		Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: t.TempDir()}}
	cache, err := newRegistryCache(t.Context(), config, "test-secret", []byte(strings.Repeat("t", 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { require.NoError(t, cache.Close()) }()
	cache.minFreeBytes = 0
	attachTestCacheControl(t, cache)
	server := httptest.NewServer(cache.httpHandler())
	defer server.Close()
	token := testRegistryToken(t, cache, "repository", configpkg.CachePolicy{Build: true})

	do := func(method, target, body string, want int) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.SetBasicAuth(Username, token)
		request.Header.Set("Content-Type", "application/octet-stream")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			data, _ := io.ReadAll(response.Body)
			require.NoError(t, response.Body.Close())
			t.Fatalf("registry status: %d %s", response.StatusCode, data)
		}
		return response
	}

	response := do(http.MethodPost, server.URL+"/v2/cache/repository/build/blobs/uploads/", "", http.StatusAccepted)
	location := response.Header.Get("Location")
	require.NoError(t, response.Body.Close())
	if location == "" {
		t.Fatal("blob upload did not return a location")
	}
	if !strings.HasPrefix(location, "http://") && !strings.HasPrefix(location, "https://") {
		location = server.URL + location
	}
	separator := "?"
	if strings.Contains(location, "?") {
		separator = "&"
	}
	content := "local cache layer"
	digestValue := digest.FromString(content).String()
	response = do(http.MethodPut, location+separator+"digest="+digestValue, content, http.StatusCreated)
	require.NoError(t, response.Body.Close())

	response = do(http.MethodGet, server.URL+"/v2/cache/repository/build/blobs/"+digestValue, "", http.StatusOK)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatalf("cached blob = %q, want %q", data, content)
	}
}

func TestRegistryCacheRequiresCredentialsForBuildKitPath(t *testing.T) {
	config := configpkg.CacheConfig{ID: "test", Name: "Test", Enabled: true,
		Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: t.TempDir()}}
	cache, err := newRegistryCache(t.Context(), config, "test-secret", []byte(strings.Repeat("t", 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { require.NoError(t, cache.Close()) }()
	cache.minFreeBytes = 0
	namespace := Namespace(cache.secret, "site-one", 2, 3)
	request := httptest.NewRequest(http.MethodPost, "/v2/cache/"+namespace+"/build/blobs/uploads/", nil)
	request.SetBasicAuth(Username, testRegistryToken(t, cache, namespace, configpkg.CachePolicy{Build: true}))
	response := httptest.NewRecorder()
	cache.httpHandler().ServeHTTP(response, request)
	if response.Code == http.StatusUnauthorized {
		t.Fatal("BuildKit request with cache credentials was rejected")
	}
	request = httptest.NewRequest(http.MethodPost, "/v2/cache/"+namespace+"/build/blobs/uploads/", nil)
	response = httptest.NewRecorder()
	cache.httpHandler().ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestRegistryCacheCleanupPreservesSharedLayers(t *testing.T) {
	ctx := t.Context()
	cache := &registryCache{enabled: true, secret: []byte("test-key"), storageDir: t.TempDir(), maxAge: 24 * time.Hour}
	attachTestCacheControl(t, cache)
	driver, err := filesystem.FromParameters(map[string]any{"rootdirectory": cache.storageDir})
	if err != nil {
		t.Fatal(err)
	}
	cache.driver = driver
	registry, err := storage.NewRegistry(ctx, driver, storage.EnableDelete)
	if err != nil {
		t.Fatal(err)
	}
	var shared digest.Digest
	for _, name := range []string{"cache/old/build", "cache/current/build"} {
		named, err := reference.WithName(name)
		if err != nil {
			t.Fatal(err)
		}
		repository, err := registry.Repository(ctx, named)
		if err != nil {
			t.Fatal(err)
		}
		config, err := repository.Blobs(ctx).Put(ctx, schema2.MediaTypeImageConfig, []byte(`{"architecture":"amd64","os":"linux"}`))
		if err != nil {
			t.Fatal(err)
		}
		layer, err := repository.Blobs(ctx).Put(ctx, schema2.MediaTypeLayer, []byte("shared layer"))
		if err != nil {
			t.Fatal(err)
		}
		shared = layer.Digest
		unique, err := repository.Blobs(ctx).Put(ctx, schema2.MediaTypeLayer, []byte(name))
		if err != nil {
			t.Fatal(err)
		}
		value, err := schema2.FromStruct(schema2.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: schema2.MediaTypeManifest,
			Config: config, Layers: []distribution.Descriptor{layer, unique},
		})
		if err != nil {
			t.Fatal(err)
		}
		manifests, err := repository.Manifests(ctx)
		if err != nil {
			t.Fatal(err)
		}
		manifestDigest, err := manifests.Put(ctx, value)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Tags(ctx).Tag(ctx, "latest", distribution.Descriptor{Digest: manifestDigest}); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	root := filepath.Join(cache.storageDir, "docker/registry/v2/repositories/cache/old")
	if err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}); err != nil {
		t.Fatal(err)
	}
	before, err := cacheBlobBytes(ctx, driver)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.prune(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := cacheBlobBytes(ctx, driver)
	if err != nil || after >= before {
		t.Fatalf("cleanup did not reclaim expired data: before=%d, after=%d, err=%v", before, after, err)
	}
	named, err := reference.WithName("cache/current/build")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.Repository(ctx, named)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Tags(ctx).Get(ctx, "latest"); err != nil {
		t.Fatalf("read retained manifest: %v", err)
	}
	if _, err := repository.Blobs(ctx).Get(ctx, shared); err != nil {
		t.Fatalf("read retained shared layer: %v", err)
	}
	if err := cache.prune(context.Background()); err != nil {
		t.Fatalf("repeat cleanup: %v", err)
	}
}

func TestRegistryCacheCleanupAcceptsEmptyStorage(t *testing.T) {
	cache := &registryCache{enabled: true, storageDir: t.TempDir()}
	driver, err := filesystem.FromParameters(map[string]any{"rootdirectory": cache.storageDir})
	if err != nil {
		t.Fatal(err)
	}
	cache.driver = driver
	if err := cache.prune(t.Context()); err != nil {
		t.Fatalf("clean empty cache: %v", err)
	}
	if cache.cleanupResult != "completed" || cache.lastCleanup.IsZero() {
		t.Fatalf("cleanup status = %q at %v", cache.cleanupResult, cache.lastCleanup)
	}
}

func TestRegistryCachePruneCanBeCanceledWhileRequestsAreActive(t *testing.T) {
	cache := &registryCache{enabled: true, storageDir: t.TempDir()}
	driver, err := filesystem.FromParameters(map[string]any{"rootdirectory": cache.storageDir})
	require.NoError(t, err)
	cache.driver = driver

	cache.maintenance.RLock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = cache.prune(ctx)
	cache.maintenance.RUnlock()

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, cache.maintenanceRequested.Load())
}

func TestRegistryCacheRepositoryAuthorization(t *testing.T) {
	t.Parallel()

	cache := &registryCache{
		secret: []byte("secret"),
		upstreams: map[string]registryCacheUpstream{
			configpkg.CacheUpstreamID("ghcr.io"): {host: "ghcr.io", allow: []string{"devcontainers/*", "team/*"}},
		},
	}
	repoHash := Namespace(cache.secret, "site-one", 2, 3)
	token := testRegistryToken(t, cache, repoHash, configpkg.CachePolicy{Public: true, Build: true})

	for _, tc := range []struct {
		name       string
		repository string
		action     string
		allowed    bool
	}{
		{name: "scoped build push", repository: "cache/" + repoHash + "/build", action: "push", allowed: true},
		{name: "allowed mirror pull", repository: testMirrorRepository("ghcr.io", "devcontainers/features/go"), action: "pull", allowed: true},
		{name: "tampered cache namespace", repository: "cache/" + repoHash[:len(repoHash)-1] + "x/build", action: "pull"},
		{name: "unknown mirror host", repository: testMirrorRepository("docker.io", "library/ubuntu"), action: "pull"},
		{name: "disallowed mirror path", repository: testMirrorRepository("ghcr.io", "other/image"), action: "pull"},
		{name: "unsupported action", repository: "cache/" + repoHash + "/build", action: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodGet
			if tc.action == "push" {
				method = http.MethodPut
			}
			request := httptest.NewRequest(method, "/v2/"+tc.repository+"/manifests/cache", nil)
			request.SetBasicAuth(Username, token)
			err := cache.authorize(request, tc.repository, tc.action)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRegistryCacheBlobMountRequiresSameRepository(t *testing.T) {
	t.Parallel()

	cache := &registryCache{
		secret: []byte("secret"),
	}
	repoHash := Namespace(cache.secret, "site-one", 2, 3)
	repository := "cache/" + repoHash + "/build"
	request, err := http.NewRequest(http.MethodPost, "/v2/"+repository+"/blobs/uploads/?mount=sha256:abc&from="+repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(Username, testRegistryToken(t, cache, repoHash, configpkg.CachePolicy{Public: true, Build: true}))
	if err := cache.authorize(request, repository, "push"); err != nil {
		t.Fatalf("authorize same-repository blob mount: %v", err)
	}

	request, err = http.NewRequest(http.MethodPost, "/v2/"+repository+"/blobs/uploads/?mount=sha256:abc&from=cache/other/build", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(Username, testRegistryToken(t, cache, repoHash, configpkg.CachePolicy{Public: true, Build: true}))
	if err := cache.authorize(request, repository, "push"); err == nil {
		t.Fatal("authorized cross-repository blob mount")
	}
}
