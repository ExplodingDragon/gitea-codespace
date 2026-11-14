// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"fmt"
	"slices"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
)

// RuntimeConfig converts the cache settings. The caller supplies component
// identity, revision, listen address and storage path before validating it.
func (cache CacheComponentSpec) RuntimeConfig() (configpkg.CacheConfig, error) {
	maxAge, err := time.ParseDuration(cache.MaxAge)
	if err != nil {
		return configpkg.CacheConfig{}, fmt.Errorf("cache maximum age: %w", err)
	}
	gcInterval, err := time.ParseDuration(cache.GCInterval)
	if err != nil {
		return configpkg.CacheConfig{}, fmt.Errorf("cache cleanup interval: %w", err)
	}
	upstreams := make(map[string]configpkg.RuntimeCacheUpstreamConfig, len(cache.Upstreams))
	for host, upstream := range cache.Upstreams {
		upstreams[host] = configpkg.RuntimeCacheUpstreamConfig{Allow: slices.Clone(upstream.Allow)}
	}
	return configpkg.CacheConfig{
		Enabled: cache.Enabled, MaxSize: cache.MaxSize,
		MaxAge: configpkg.Duration(maxAge), GCInterval: configpkg.Duration(gcInterval), Upstreams: upstreams,
		Storage: configpkg.CacheStorageConfig{
			Driver: cache.Storage.Driver, MinFreeSpace: cache.Storage.MinFreeSpace,
			S3: configpkg.CacheS3Config{
				Endpoint: cache.Storage.S3.Endpoint, Region: cache.Storage.S3.Region, Bucket: cache.Storage.S3.Bucket,
				Prefix: cache.Storage.S3.Prefix, ForcePathStyle: cache.Storage.S3.ForcePathStyle,
			},
		},
	}, nil
}
