// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"
)

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
