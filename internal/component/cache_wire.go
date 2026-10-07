// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package component

import (
	"fmt"
	"time"

	componentv1 "gitea.dev/codespace-proto-go/component/v1"
	cachepkg "gitea.dev/codespace/internal/cache"
	configpkg "gitea.dev/codespace/internal/config"
)

func CacheConfigToProto(config configpkg.CacheConfig) *componentv1.CacheConfiguration {
	result := &componentv1.CacheConfiguration{
		Id: config.ID, Name: config.Name, Revision: config.Revision, Enabled: config.Enabled,
		Listen: config.Listen, PublicUrl: config.PublicURL, MaxSize: config.MaxSize,
		MaxAgeMilliseconds: time.Duration(config.MaxAge).Milliseconds(), GcIntervalMilliseconds: time.Duration(config.GCInterval).Milliseconds(),
		Storage:   &componentv1.CacheStorageConfiguration{Driver: config.Storage.Driver, Path: config.Storage.Path, MinFreeSpace: config.Storage.MinFreeSpace},
		Upstreams: make(map[string]*componentv1.CacheUpstreamConfiguration, len(config.Upstreams)),
	}
	if config.Storage.Driver == "s3" {
		result.Storage.S3 = &componentv1.CacheS3Configuration{
			Endpoint: config.Storage.S3.Endpoint, Region: config.Storage.S3.Region, Bucket: config.Storage.S3.Bucket,
			Prefix: config.Storage.S3.Prefix, ForcePathStyle: config.Storage.S3.ForcePathStyle,
		}
	}
	for host, upstream := range config.Upstreams {
		result.Upstreams[host] = &componentv1.CacheUpstreamConfiguration{Allow: append([]string(nil), upstream.Allow...)}
	}
	return result
}

func cacheConfigFromProto(config *componentv1.CacheConfiguration) (configpkg.CacheConfig, error) {
	if config == nil || config.Storage == nil {
		return configpkg.CacheConfig{}, fmt.Errorf("cache configuration is incomplete")
	}
	result := configpkg.CacheConfig{
		ID: config.Id, Name: config.Name, Revision: config.Revision, Enabled: config.Enabled, Listen: config.Listen,
		PublicURL: config.PublicUrl, MaxSize: config.MaxSize,
		MaxAge:     configpkg.Duration(time.Duration(config.MaxAgeMilliseconds) * time.Millisecond),
		GCInterval: configpkg.Duration(time.Duration(config.GcIntervalMilliseconds) * time.Millisecond),
		Storage:    configpkg.CacheStorageConfig{Driver: config.Storage.Driver, Path: config.Storage.Path, MinFreeSpace: config.Storage.MinFreeSpace},
		Upstreams:  make(map[string]configpkg.RuntimeCacheUpstreamConfig, len(config.Upstreams)),
	}
	if config.Storage.S3 != nil {
		result.Storage.S3 = configpkg.CacheS3Config{
			Endpoint: config.Storage.S3.Endpoint, Region: config.Storage.S3.Region, Bucket: config.Storage.S3.Bucket,
			Prefix: config.Storage.S3.Prefix, ForcePathStyle: config.Storage.S3.ForcePathStyle,
		}
	}
	for host, upstream := range config.Upstreams {
		if upstream == nil {
			return configpkg.CacheConfig{}, fmt.Errorf("cache upstream %q is missing", host)
		}
		result.Upstreams[host] = configpkg.RuntimeCacheUpstreamConfig{Allow: append([]string(nil), upstream.Allow...)}
	}
	return result, nil
}

func cacheStatusToProto(status cachepkg.Status) *componentv1.CacheStatus {
	return &componentv1.CacheStatus{
		PublicUrl: status.PublicURL, ConfigRevision: status.ConfigRevision, CacheBytes: status.CacheBytes,
		MirrorBytes: status.MirrorBytes, ScannedAtUnixMilliseconds: status.ScannedAt.UnixMilli(),
		LastCleanupUnixMilliseconds: status.LastCleanup.UnixMilli(), CleanupResult: status.CleanupResult,
	}
}

func cacheGCGrantFromProto(grant *componentv1.CacheGCGrant) (cachepkg.GCGrant, error) {
	if grant == nil || grant.CacheId == "" || grant.Id == "" {
		return cachepkg.GCGrant{}, fmt.Errorf("cache GC grant is incomplete")
	}
	return cachepkg.GCGrant{CacheID: grant.CacheId, ID: grant.Id, ConfigRevision: grant.ConfigRevision, ExpiresAt: time.UnixMilli(grant.ExpiresAtUnixMilliseconds)}, nil
}
