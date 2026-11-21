// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"gitea.dev/codespace/internal/healthcheck"
	"github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	"github.com/stretchr/testify/require"
)

func TestCacheAuthenticationExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		key := []byte(strings.Repeat("t", 32))
		reader := &registryCache{id: "test", tokenKey: key}
		token, err := SignCredential(key, "test", "repository", configpkg.CachePolicy{Build: true}, time.Now().Add(time.Second))
		require.NoError(t, err)
		_, err = reader.verifyToken(token)
		require.NoError(t, err)
		time.Sleep(time.Second / 2)
		_, err = reader.verifyToken(token)
		require.NoError(t, err)
		_, err = reader.verifyToken(token + "x")
		require.Error(t, err)
		time.Sleep(time.Second / 2)
		_, err = reader.verifyToken(token)
		require.Error(t, err, "a cached credential expires at its original deadline")
	})
}

func TestCacheServiceStopsMaintenanceWhenManagerLeaseIsLost(t *testing.T) {
	control := &fakeCacheControl{
		config:       ControlConfig{Config: configpkg.CacheConfig{ID: "test", Enabled: true, Listen: "127.0.0.1:0", Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: t.TempDir()}}, RegistryKey: "test-key", TokenKey: []byte(strings.Repeat("t", 32))},
		heartbeatErr: errors.New("cache service ownership lost"),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	tlsConfig := testCacheTLSConfig(t)
	go func() {
		done <- runCacheInstance(ctx, io.Discard, control, control.config, tlsConfig, &healthcheck.State{})
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != "cache service ownership lost" {
			t.Fatalf("runCacheInstance error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("cache instance did not stop after manager lease loss")
	}
}

func TestCacheServiceExpiresWhileHeartbeatIsBlocked(t *testing.T) {
	control := &fakeCacheControl{
		config:         ControlConfig{Config: configpkg.CacheConfig{ID: "test", Enabled: true, Listen: "127.0.0.1:0", Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: t.TempDir()}}, RegistryKey: "test-key", TokenKey: []byte(strings.Repeat("t", 32))},
		permitDuration: 50 * time.Millisecond, stallHeartbeat: true,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	released := make(chan struct{})
	tlsConfig := testCacheTLSConfig(t)
	go func() {
		_ = runCacheInstance(ctx, io.Discard, control, control.config, tlsConfig, &healthcheck.State{})
		close(released)
	}()
	select {
	case <-released:
		control.mu.Lock()
		defer control.mu.Unlock()
		if control.owner.ID != "" {
			t.Fatal("expired cache owner was not released")
		}
	case <-ctx.Done():
		t.Fatal("blocked heartbeat prevented the cache permit from expiring")
	}
}

func testCacheTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	return server.TLS.Clone()
}

// fakeCacheControl models the Manager contract. Manager integration tests cover
// lease and revision conditions against Kubernetes resources.
type fakeCacheControl struct {
	mu             sync.Mutex
	config         ControlConfig
	owner          Owner
	heartbeatErr   error
	permitDuration time.Duration
	stallHeartbeat bool
	gcGrant        GCGrant
	beginSignal    chan struct{}
	completeSignal chan struct{}
	completeCount  int
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
		return Owner{}, errCacheServiceBusy
	}
	duration := f.permitDuration
	if duration == 0 {
		duration = 15 * time.Second
	}
	f.owner = Owner{CacheID: id, ID: "owner", ExpiresAt: time.Now().Add(duration)}
	return f.owner, nil
}

func (f *fakeCacheControl) HeartbeatCache(ctx context.Context, owner Owner, _ Status) (time.Time, error) {
	if f.stallHeartbeat {
		<-ctx.Done()
		return time.Time{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner.ID == "" || owner.ID != f.owner.ID {
		return time.Time{}, errors.New("cache service ownership lost")
	}
	if f.heartbeatErr != nil {
		return time.Time{}, f.heartbeatErr
	}
	return time.Now().Add(15 * time.Second), nil
}

func (f *fakeCacheControl) ReleaseCache(_ context.Context, owner Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner.ID == f.owner.ID {
		f.owner = Owner{}
	}
	return nil
}

func (f *fakeCacheControl) BeginCacheGC(context.Context, Owner, bool) (GCGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beginSignal != nil {
		close(f.beginSignal)
		f.beginSignal = nil
	}
	return f.gcGrant, nil
}

func (f *fakeCacheControl) CompleteCacheGC(context.Context, GCGrant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCount++
	f.gcGrant = GCGrant{}
	if f.completeSignal != nil {
		close(f.completeSignal)
		f.completeSignal = nil
	}
	return nil
}

func TestCacheMaintenanceGrant(t *testing.T) {
	for _, complete := range []bool{false, true} {
		name := "canceled"
		if complete {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) {
			driver, err := filesystem.FromParameters(map[string]any{"rootdirectory": t.TempDir()})
			require.NoError(t, err)
			synctest.Test(t, func(t *testing.T) {
				readyAt := time.Now().Add(time.Minute)
				control := &fakeCacheControl{
					config:      ControlConfig{Config: configpkg.CacheConfig{ID: "test", Revision: 1, Enabled: true}},
					gcGrant:     GCGrant{CacheID: "test", ID: "grant", ReadyAt: readyAt, ExpiresAt: readyAt.Add(time.Minute)},
					beginSignal: make(chan struct{}), completeSignal: make(chan struct{}),
				}
				beginSignal, completeSignal := control.beginSignal, control.completeSignal
				cache := &registryCache{id: "test", configRevision: 1, gcInterval: time.Hour, driver: driver}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan struct{})
				go func() { cache.maintain(ctx, control); close(done) }()
				<-beginSignal
				synctest.Wait()
				control.mu.Lock()
				completed := control.completeCount
				control.mu.Unlock()
				require.Zero(t, completed)
				if complete {
					<-completeSignal
					require.Equal(t, readyAt, time.Now())
				}
				cancel()
				<-done
				if complete {
					require.Equal(t, 1, control.completeCount)
					require.Empty(t, control.gcGrant.ID)
				} else {
					require.Zero(t, control.completeCount)
					require.Equal(t, "grant", control.gcGrant.ID)
				}
			})
		})
	}
}
