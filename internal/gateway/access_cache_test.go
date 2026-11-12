// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGatewaySharedAuthorizationHasIndependentWaiters(t *testing.T) {
	for _, cached := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			controller := newAccessController(accessConfig{ValidationMaxInflight: 1})
			key := gatewayAuthorizationKey{kind: gatewayAuthorizationKindEndpoint, siteUID: "site", userID: 1, codespaceUUID: "runtime", endpointID: "web"}
			complete := make(chan struct{})
			calls := 0
			validate := func(ctx context.Context) (gatewayAccessDecision, error) {
				calls++
				select {
				case <-complete:
					return gatewayAccessDecision{Allowed: true}, nil
				case <-ctx.Done():
					return gatewayAccessDecision{}, ctx.Err()
				}
			}
			lookup := func(ctx context.Context) error {
				var decision gatewayAccessDecision
				var full bool
				var err error
				if cached {
					decision, full, err = controller.validateAccess(ctx, key, time.Now(), validate)
				} else {
					decision, full, err = controller.validateAccessUncached(ctx, key, validate)
				}
				if full || (err == nil && !decision.Allowed) {
					return fmt.Errorf("unexpected authorization result: full=%t decision=%+v", full, decision)
				}
				return err
			}
			leader, cancel := context.WithCancel(t.Context())
			defer cancel()
			leaderResult := make(chan error, 1)
			go func() { leaderResult <- lookup(leader) }()
			synctest.Wait()
			followerResult := make(chan error, 1)
			go func() { followerResult <- lookup(t.Context()) }()
			synctest.Wait()
			cancel()
			require.ErrorIs(t, <-leaderResult, context.Canceled)
			close(complete)
			require.NoError(t, <-followerResult)
			require.Equal(t, 1, calls)
			require.Equal(t, cached, controller.cache.IsAllowed(key, time.Now()))
		})
	}
}

func TestGatewaySharedAuthorizationTimeoutReleasesCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		controller := newAccessController(accessConfig{ValidationMaxInflight: 1})
		key := gatewayAuthorizationKey{kind: gatewayAuthorizationKindPublic, siteUID: "site", codespaceUUID: "runtime", endpointID: "web"}
		_, full, err := controller.validateAccessUncached(t.Context(), key, func(ctx context.Context) (gatewayAccessDecision, error) {
			<-ctx.Done()
			return gatewayAccessDecision{}, ctx.Err()
		})
		require.False(t, full)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		decision, full, err := controller.validateAccessUncached(t.Context(), key, func(context.Context) (gatewayAccessDecision, error) {
			return gatewayAccessDecision{Allowed: true}, nil
		})
		require.NoError(t, err)
		require.False(t, full)
		require.True(t, decision.Allowed)
	})
}

func TestGatewayAccessStateIsScopedBySite(t *testing.T) {
	controller := newAccessController(accessConfig{
		AllowedTTL:                      time.Minute,
		MaxInflightTotal:                4,
		PublicMaxConnectionsPerEndpoint: 1,
		PublicMaxConnectionsPerIP:       4,
		ValidationMaxInflight:           4,
	})
	now := time.Unix(100, 0)
	validations := 0
	validate := func(context.Context) (gatewayAccessDecision, error) {
		validations++
		return gatewayAccessDecision{Allowed: true}, nil
	}
	for _, siteUID := range []string{"site-1", "site-1", "site-2"} {
		decision, full, err := controller.ValidatePublicEndpoint(t.Context(), siteUID, "runtime", "web", now, validate)
		if err != nil || full || !decision.Allowed {
			t.Fatalf("validate site %q: decision=%#v full=%v err=%v", siteUID, decision, full, err)
		}
	}
	if validations != 2 {
		t.Fatalf("validation calls = %d, want 2", validations)
	}

	first, status := controller.reservePublic("site-1", "runtime", "web", "192.0.2.1")
	if status != 0 {
		t.Fatalf("reserve first site: status %d", status)
	}
	defer first.Release()
	if _, status := controller.reservePublic("site-1", "runtime", "web", "192.0.2.2"); status != http.StatusTooManyRequests {
		t.Fatalf("reserve duplicate site: status %d", status)
	}
	second, status := controller.reservePublic("site-2", "runtime", "web", "192.0.2.2")
	if status != 0 {
		t.Fatalf("reserve second site: status %d", status)
	}
	second.Release()
}

func TestGatewayAccessControllerUsesPositiveAuthorizationTTL(t *testing.T) {
	controller := newAccessControllerFromConfig(validGatewayConfig())
	if controller.Config.AllowedTTL != 30*time.Second {
		t.Fatalf("allowed TTL = %s, want 30s", controller.Config.AllowedTTL)
	}
}

func TestGatewayAccessCachePrunesExpiredBeforeApplyingLimit(t *testing.T) {
	cache := newGatewayAccessCache(time.Minute)
	cache.maxKeys = 2
	now := time.Unix(100, 0)
	expiredKey := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindEndpoint,
		userID:        1,
		codespaceUUID: "codespace-expired",
		endpointID:    "web",
	}
	currentKey := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindEndpoint,
		userID:        1,
		codespaceUUID: "codespace-current",
		endpointID:    "web",
	}
	newKey := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindEndpoint,
		userID:        1,
		codespaceUUID: "codespace-new",
		endpointID:    "web",
	}
	cache.allowed[expiredKey] = now.Add(-time.Second)
	cache.allowed[currentKey] = now.Add(time.Minute)

	cache.MarkAllowed(newKey, now)

	if len(cache.allowed) != 2 {
		t.Fatalf("allowed cache size = %d, want 2", len(cache.allowed))
	}
	if cache.IsAllowed(expiredKey, now) {
		t.Fatalf("expired key remained allowed")
	}
	if !cache.IsAllowed(currentKey, now) {
		t.Fatalf("current key was pruned")
	}
	if !cache.IsAllowed(newKey, now) {
		t.Fatalf("new key was not allowed")
	}
}

func TestGatewayAccessCacheEvictsOldestWhenFull(t *testing.T) {
	cache := newGatewayAccessCache(time.Minute)
	cache.maxKeys = 2
	now := time.Unix(100, 0)
	oldKey := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindEndpoint,
		userID:        1,
		codespaceUUID: "codespace-old",
		endpointID:    "web",
	}
	currentKey := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindEndpoint,
		userID:        1,
		codespaceUUID: "codespace-current",
		endpointID:    "web",
	}
	newKey := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindEndpoint,
		userID:        1,
		codespaceUUID: "codespace-new",
		endpointID:    "web",
	}
	cache.allowed[oldKey] = now.Add(10 * time.Second)
	cache.allowed[currentKey] = now.Add(30 * time.Second)

	cache.MarkAllowed(newKey, now)

	if len(cache.allowed) != 2 {
		t.Fatalf("allowed cache size = %d, want 2", len(cache.allowed))
	}
	if cache.IsAllowed(oldKey, now) {
		t.Fatalf("oldest key remained allowed")
	}
	if !cache.IsAllowed(currentKey, now) {
		t.Fatalf("current key was pruned")
	}
	if !cache.IsAllowed(newKey, now) {
		t.Fatalf("new key was not allowed")
	}
}
