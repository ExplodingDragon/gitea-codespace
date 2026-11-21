// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/manifest/manifestlist"
	"github.com/distribution/distribution/v3/manifest/schema2"
	"github.com/distribution/distribution/v3/registry/storage"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	"github.com/stretchr/testify/require"
)

func testMirrorRepository(host, image string) string {
	return "mirror/" + configpkg.CacheUpstreamID(host) + "/" + image
}

func testRegistryToken(t *testing.T, cache *registryCache, namespace string, policy configpkg.CachePolicy) string {
	t.Helper()
	if cache.id == "" {
		cache.id = "test"
	}
	if len(cache.tokenKey) == 0 {
		cache.tokenKey = []byte(strings.Repeat("t", 32))
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
	request = httptest.NewRequest(http.MethodGet, strings.TrimSuffix(path, "latest")+previous, nil)
	request.Header.Set("Accept", schema2.MediaTypeManifest)
	request.SetBasicAuth(Username, testRegistryToken(t, cache, "repository", configpkg.CachePolicy{Public: true}))
	response = httptest.NewRecorder()
	cache.httpHandler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, previous, response.Header().Get("Docker-Content-Digest"))
}

func TestRegistryCacheServesScopedBuildBlobs(t *testing.T) {
	config := configpkg.CacheConfig{ID: "test", Name: "Test", Enabled: true, Listen: "127.0.0.1:0",
		Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: t.TempDir()}}
	cache, err := newRegistryCache(t.Context(), config, "test-secret", []byte(strings.Repeat("t", 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { require.NoError(t, cache.Close()) }()
	server := httptest.NewServer(cache.httpHandler())
	defer server.Close()
	token := testRegistryToken(t, cache, "repository", configpkg.CachePolicy{Build: true})
	unauthenticated := httptest.NewRecorder()
	cache.httpHandler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/v2/cache/repository/build/blobs/uploads/", nil))
	require.Equal(t, http.StatusUnauthorized, unauthenticated.Code)

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
	response = do(http.MethodPost, server.URL+"/v2/cache/repository/features/blobs/uploads/?mount="+digestValue+"&from=cache/repository/build", "", http.StatusCreated)
	require.NoError(t, response.Body.Close())
	response = do(http.MethodGet, server.URL+"/v2/cache/repository/features/blobs/"+digestValue, "", http.StatusOK)
	require.NoError(t, response.Body.Close())
	for _, source := range []string{"cache/another/build", "cache/repository/build/../other", "cache/repository/../../other", "cache/repository/build:tag"} {
		query := url.Values{"mount": {digestValue}, "from": {source}}
		response = do(http.MethodPost, server.URL+"/v2/cache/repository/features/blobs/uploads/?"+query.Encode(), "", http.StatusUnauthorized)
		require.NoError(t, response.Body.Close())
	}
}

func TestRegistryCacheCleanupPreservesSharedLayers(t *testing.T) {
	for _, mirror := range []bool{false, true} {
		t.Run(fmt.Sprintf("mirror=%t", mirror), func(t *testing.T) {
			ctx := t.Context()
			cache := &registryCache{enabled: true, storageDir: t.TempDir(), maxAge: 24 * time.Hour}
			storageRoot := cache.storageDir
			driver, err := filesystem.FromParameters(map[string]any{"rootdirectory": storageRoot})
			require.NoError(t, err)
			cache.driver = driver
			if mirror {
				storageRoot = t.TempDir()
				driver, err = filesystem.FromParameters(map[string]any{"rootdirectory": storageRoot})
				require.NoError(t, err)
				cache.mirrorDrivers = map[string]storagedriver.StorageDriver{"registry.example.com": driver}
			}
			registry, err := storage.NewRegistry(ctx, driver, storage.EnableDelete)
			require.NoError(t, err)
			var shared, obsolete, child digest.Digest
			for _, name := range []string{"cache/untagged/build", "cache/old/build", "cache/current/build"} {
				named, err := reference.WithName(name)
				require.NoError(t, err)
				repository, err := registry.Repository(ctx, named)
				require.NoError(t, err)
				config, err := repository.Blobs(ctx).Put(ctx, schema2.MediaTypeImageConfig, []byte(`{"architecture":"amd64","os":"linux"}`))
				require.NoError(t, err)
				layer, err := repository.Blobs(ctx).Put(ctx, schema2.MediaTypeLayer, []byte("shared layer"))
				require.NoError(t, err)
				shared = layer.Digest
				manifests, err := repository.Manifests(ctx)
				require.NoError(t, err)
				for _, version := range []string{"obsolete", "current"} {
					unique, err := repository.Blobs(ctx).Put(ctx, schema2.MediaTypeLayer, []byte(name+version))
					require.NoError(t, err)
					value, err := schema2.FromStruct(schema2.Manifest{
						Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: schema2.MediaTypeManifest,
						Config: config, Layers: []distribution.Descriptor{layer, unique},
					})
					require.NoError(t, err)
					valueDigest, err := manifests.Put(ctx, value)
					require.NoError(t, err)
					if version == "obsolete" {
						obsolete = valueDigest
						// Tag history makes this a real replaced build result.
						if name != "cache/untagged/build" {
							require.NoError(t, repository.Tags(ctx).Tag(ctx, "latest", distribution.Descriptor{Digest: valueDigest}))
						}
						continue
					}
					child = valueDigest
					index, err := manifestlist.FromDescriptors([]manifestlist.ManifestDescriptor{{
						Descriptor: distribution.Descriptor{Digest: valueDigest, MediaType: schema2.MediaTypeManifest},
						Platform:   manifestlist.PlatformSpec{Architecture: "amd64", OS: "linux"},
					}})
					require.NoError(t, err)
					indexDigest, err := manifests.Put(ctx, index)
					require.NoError(t, err)
					if name != "cache/untagged/build" {
						require.NoError(t, repository.Tags(ctx).Tag(ctx, "latest", distribution.Descriptor{Digest: indexDigest}))
					}
				}
			}
			ageRepository := func(name string, age time.Duration) {
				t.Helper()
				old := time.Now().Add(-age)
				require.NoError(t, driver.Walk(ctx, "/docker/registry/v2/repositories/"+name+"/_manifests", func(info storagedriver.FileInfo) error {
					// This test uses filesystem storage so link timestamps can be aged deterministically.
					return os.Chtimes(filepath.Join(storageRoot, info.Path()), old, old)
				}))
			}
			ageRepository("cache/old/build", 48*time.Hour)
			scan, err := cache.scanStorage(ctx)
			require.NoError(t, err)
			require.True(t, scan.needsCleanup)
			before := cache.status.CacheBytes + cache.status.MirrorBytes
			require.Positive(t, before)
			if mirror {
				require.Equal(t, before, cache.status.MirrorBytes)
			}
			require.NoError(t, cache.prune(ctx))
			after, err := cacheBlobBytes(ctx, driver)
			require.NoError(t, err)
			require.Less(t, after, before)
			named, err := reference.WithName("cache/current/build")
			require.NoError(t, err)
			repository, err := registry.Repository(ctx, named)
			require.NoError(t, err)
			_, err = repository.Tags(ctx).Get(ctx, "latest")
			require.NoError(t, err)
			_, err = repository.Blobs(ctx).Get(ctx, shared)
			require.NoError(t, err)
			manifests, err := repository.Manifests(ctx)
			require.NoError(t, err)
			exists, err := manifests.Exists(ctx, child)
			require.NoError(t, err)
			require.True(t, exists, "an index still references the child")
			exists, err = manifests.Exists(ctx, obsolete)
			require.NoError(t, err)
			require.Equal(t, mirror, exists, "mirrors retain digest-only manifests")
			scan, err = cache.scanStorage(ctx)
			require.NoError(t, err)
			require.False(t, scan.needsCleanup)
			cache.maxBytes = after - 1
			ageRepository("cache/current/build", 2*time.Hour)
			if mirror {
				ageRepository("cache/untagged/build", 2*time.Hour)
			}
			scan, err = cache.scanStorage(ctx)
			require.NoError(t, err)
			require.True(t, scan.needsCleanup, "capacity includes mirrors")
			require.NoError(t, cache.prune(ctx))
			require.Zero(t, cache.status.CacheBytes+cache.status.MirrorBytes)
			require.NoError(t, cache.prune(ctx))
		})
	}
}

func TestRegistryCacheScansAndCleansUnreferencedStorage(t *testing.T) {
	cache := &registryCache{enabled: true, storageDir: t.TempDir()}
	driver, err := filesystem.FromParameters(map[string]any{"rootdirectory": cache.storageDir})
	if err != nil {
		t.Fatal(err)
	}
	cache.driver = driver
	scan, err := cache.scanStorage(t.Context())
	require.NoError(t, err)
	require.False(t, scan.needsCleanup)
	if err := cache.prune(t.Context()); err != nil {
		t.Fatalf("clean empty cache: %v", err)
	}
	if cache.status.CleanupResult != "completed" || cache.lastCleanup.IsZero() {
		t.Fatalf("cleanup status = %q at %v", cache.status.CleanupResult, cache.lastCleanup)
	}
	value := digest.FromString("orphan")
	blobPath := "/docker/registry/v2/blobs/sha256/" + value.Encoded()[:2] + "/" + value.Encoded() + "/data"
	require.NoError(t, driver.PutContent(t.Context(), blobPath, []byte("orphan")))
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(cache.storageDir, blobPath), old, old))
	scan, err = cache.scanStorage(t.Context())
	require.NoError(t, err)
	require.True(t, scan.needsCleanup)
	require.EqualValues(t, len("orphan"), cache.status.CacheBytes)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = cache.scanStorage(ctx)
	require.Error(t, err)
	require.EqualValues(t, len("orphan"), cache.status.CacheBytes, "failed scans preserve the last complete snapshot")
	require.NoError(t, cache.prune(t.Context()))
	require.Zero(t, cache.status.CacheBytes)
	uploadRoot := "/docker/registry/v2/repositories/cache/unfinished/_uploads/11111111-1111-4111-8111-111111111111"
	require.NoError(t, driver.PutContent(t.Context(), uploadRoot+"/startedat", []byte(time.Now().Add(-48*time.Hour).Format(time.RFC3339))))
	require.NoError(t, driver.PutContent(t.Context(), uploadRoot+"/data", []byte("partial upload")))
	scan, err = cache.scanStorage(t.Context())
	require.NoError(t, err)
	require.True(t, scan.needsCleanup)
	require.NoError(t, cache.prune(t.Context()))
	_, err = driver.Stat(t.Context(), uploadRoot)
	require.ErrorAs(t, err, new(storagedriver.PathNotFoundError))
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
