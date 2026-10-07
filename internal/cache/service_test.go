// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
)

func TestCacheAuthenticationExpiry(t *testing.T) {
	key := []byte(strings.Repeat("t", 32))
	reader := &registryCache{id: "test", tokenKey: key}
	token, err := SignCredential(key, "test", "repository", configpkg.CachePolicy{Build: true}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.verifyToken(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	hashKey := cacheSessionKey(token)
	expires := reader.sessions[hashKey].expires
	if _, err := reader.verifyToken(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if !reader.sessions[hashKey].expires.Equal(expires) {
		t.Fatal("cache hit extended credential lifetime")
	}
	entry := reader.sessions[hashKey]
	entry.expires = time.Now().Add(-time.Second)
	reader.sessions[hashKey] = entry
	if _, err := reader.verifyToken(t.Context(), token+"x"); err == nil {
		t.Fatal("modified cache credential was accepted")
	}
}

func TestCacheServiceStopsMaintenanceWhenManagerLeaseIsLost(t *testing.T) {
	control := &fakeCacheControl{
		config:       ControlConfig{Config: configpkg.CacheConfig{ID: "test", Enabled: true, Listen: "127.0.0.1:0", PublicURL: "http://cache.example.com", Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: t.TempDir()}}, RegistryKey: "test-key", TokenKey: []byte(strings.Repeat("t", 32))},
		heartbeatErr: errors.New("cache service ownership lost"),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runCacheInstance(ctx, io.Discard, control, control.config) }()
	select {
	case err := <-done:
		if err == nil || err.Error() != "cache service ownership lost" {
			t.Fatalf("runCacheInstance error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("cache instance did not stop after manager lease loss")
	}
}

func cacheSessionKey(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

// fakeCacheControl models the Manager contract. Manager integration tests cover
// lease and revision conditions against Kubernetes resources.
type fakeCacheControl struct {
	mu           sync.Mutex
	config       ControlConfig
	owner        Owner
	heartbeatErr error
}

func (f *fakeCacheControl) GetCache(context.Context, string) (ControlConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.config, nil
}

func (f *fakeCacheControl) ClaimCache(_ context.Context, id string) (Owner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owner.ID != "" {
		return Owner{}, ErrCacheServiceBusy
	}
	f.owner = Owner{CacheID: id, ID: "owner", ConfigRevision: f.config.Config.Revision}
	return f.owner, nil
}

func (f *fakeCacheControl) HeartbeatCache(_ context.Context, owner Owner, _ Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner.ID == "" || owner.ID != f.owner.ID {
		return errors.New("cache service ownership lost")
	}
	if f.heartbeatErr != nil {
		return f.heartbeatErr
	}
	return nil
}

func (f *fakeCacheControl) ReleaseCache(_ context.Context, owner Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner.ID == f.owner.ID {
		f.owner = Owner{}
	}
	return nil
}

func (f *fakeCacheControl) BeginCacheGC(context.Context, Owner) (GCGrant, error) {
	return GCGrant{CacheID: f.owner.CacheID, ID: "gc"}, nil
}

func (f *fakeCacheControl) CompleteCacheGC(context.Context, GCGrant) error { return nil }
