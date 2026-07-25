// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"testing"
	"time"
)

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
