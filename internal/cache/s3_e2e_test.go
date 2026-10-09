// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

func TestCacheS3E2E(t *testing.T) {
	if os.Getenv("CODESPACE_E2E") != "1" {
		t.Skip("run make test-e2e to verify the S3 cache backend")
	}
	bucket := os.Getenv("CODESPACE_E2E_S3_BUCKET")
	require.NotEmpty(t, bucket)
	for _, name := range []string{"CODESPACE_E2E_S3_REGION", "CODESPACE_E2E_S3_ENDPOINT", "CODESPACE_E2E_S3_ACCESS_KEY", "CODESPACE_E2E_S3_SECRET_KEY"} {
		require.NotEmpty(t, os.Getenv(name), "%s must be set by the E2E runner", name)
	}
	config := configpkg.CacheConfig{ID: "s3-test", Name: "S3 test", Enabled: true, PublicURL: "http://cache.example.com", Storage: configpkg.CacheStorageConfig{Driver: "s3", S3: configpkg.CacheS3Config{
		Bucket: bucket, Region: os.Getenv("CODESPACE_E2E_S3_REGION"), Endpoint: os.Getenv("CODESPACE_E2E_S3_ENDPOINT"),
		AccessKey: os.Getenv("CODESPACE_E2E_S3_ACCESS_KEY"), SecretKey: os.Getenv("CODESPACE_E2E_S3_SECRET_KEY"),
		ForcePathStyle: true, Prefix: "codespace-test-" + strings.ToLower(rand.Text()),
	}}}
	cache, err := New(t.Context(), config, "test-key", []byte(strings.Repeat("t", 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { require.NoError(t, cache.Close()) }()
	// This driver is rooted in a random test-only prefix, never the bucket root.
	defer func() {
		if err := cache.driver.Delete(t.Context(), "/docker"); err != nil {
			t.Error(err)
		}
	}()
	attachTestCacheControl(t, cache)
	server := httptest.NewServer(cache.Handler())
	defer server.Close()
	token := testRegistryToken(t, cache, "repo", configpkg.CachePolicy{Build: true})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, target, body string, want int) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.SetBasicAuth(Username, token)
		request.Header.Set("Content-Type", "application/octet-stream")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			require.NoError(t, response.Body.Close())
			t.Fatalf("registry status: %d", response.StatusCode)
		}
		return response
	}
	response := do(http.MethodPost, server.URL+"/v2/cache/repo/build/blobs/uploads/", "", http.StatusAccepted)
	location := response.Header.Get("Location")
	require.NoError(t, response.Body.Close())
	content := "S3 cache layer"
	response = do(http.MethodPut, location+"&digest="+digest.FromString(content).String(), content, http.StatusCreated)
	require.NoError(t, response.Body.Close())
	response = do(http.MethodGet, server.URL+"/v2/cache/repo/build/blobs/"+digest.FromString(content).String(), "", http.StatusOK)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	if err != nil || string(data) != content {
		t.Fatalf("S3 cached content: %q, %v", data, err)
	}
	bytes, err := cacheBlobBytes(t.Context(), cache.driver)
	if err != nil || bytes < int64(len(content)) {
		t.Fatalf("S3 usage: %d, %v", bytes, err)
	}
	if err := cache.prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	bytes, err = cacheBlobBytes(t.Context(), cache.driver)
	if err != nil || bytes != 0 {
		t.Fatalf("S3 orphan cleanup: %d, %v", bytes, err)
	}
}
