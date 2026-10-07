// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"context"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
)

// Control is the Manager-owned control boundary used by a registry cache.
//
// The cache deliberately receives business operations rather than storage
// access. Manager is responsible for leases, revisions, credentials and
// conditional updates; the cache only serves registry traffic and performs
// storage maintenance with the grants it receives.
type Control interface {
	GetCache(context.Context, string) (ControlConfig, error)
	ClaimCache(context.Context, string) (Owner, error)
	HeartbeatCache(context.Context, Owner, Status) error
	ReleaseCache(context.Context, Owner) error
	BeginCacheGC(context.Context, Owner) (GCGrant, error)
	CompleteCacheGC(context.Context, GCGrant) error
}

// Session is the Manager-approved scope for one registry credential.
// It is intentionally returned only after the cache ID has been checked.
type Session struct {
	CacheID   string                `json:"cache_id"`
	Namespace string                `json:"namespace"`
	Policy    configpkg.CachePolicy `json:"policy"`
	ExpiresAt time.Time             `json:"expires_at"`
}

// ControlConfig contains the only state a cache needs to start serving.
// RegistryKey is the Distribution HTTP secret and is never persisted by the
// cache process.
type ControlConfig struct {
	Config      configpkg.CacheConfig
	RegistryKey string
	TokenKey    []byte
}

// Owner is an opaque Manager lease handle. The cache must return it
// unchanged for heartbeats and release; it cannot create or extend a lease.
type Owner struct {
	CacheID        string
	ID             string
	ConfigRevision int64
	ExpiresAt      time.Time
}

// Status is the small, manager-visible status snapshot for one cache.
type Status struct {
	PublicURL      string
	ConfigRevision int64
	CacheBytes     int64
	MirrorBytes    int64
	ScannedAt      time.Time
	LastCleanup    time.Time
	CleanupResult  string
}

// GCGrant authorizes one local storage collection pass.
type GCGrant struct {
	CacheID        string
	ID             string
	ConfigRevision int64
	ExpiresAt      time.Time
}
