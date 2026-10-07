// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config_test

import (
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGatewayConfigValidation(t *testing.T) {
	t.Parallel()

	config := configpkg.DefaultGatewayConfig()
	require.NoError(t, config.Validate())

	config.Limits.MaxInflightTotal = 4
	config.Limits.MaxInflightPerSession = 5
	require.ErrorContains(t, config.Validate(), "not exceed the total")

	config = configpkg.DefaultGatewayConfig()
	config.HTTP.PublicURL = "http://127.0.0.1:18081"
	require.ErrorContains(t, config.Validate(), "gateway.http.public_url")

	config = configpkg.DefaultGatewayConfig()
	config.SSH.HandshakeTimeout = configpkg.Duration(time.Millisecond)
	require.ErrorContains(t, config.Validate(), "handshake_timeout")
}

func TestCacheConfigValidation(t *testing.T) {
	t.Parallel()

	config := configpkg.CacheConfig{
		ID: "test", Name: "Test", Revision: 1, Enabled: true, Listen: "127.0.0.1:15000", PublicURL: "http://cache.example.com",
		Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: "cache-registry"}, MaxSize: "10GiB",
		Upstreams: map[string]configpkg.RuntimeCacheUpstreamConfig{"ghcr.io": {Allow: []string{"devcontainers/*"}}},
	}
	require.NoError(t, config.Validate())
	config.Revision = 0
	require.ErrorContains(t, config.Validate(), "revision must be positive")
	config.Revision = 1

	config.PublicURL = "https://registry.example.com/cache"
	require.ErrorContains(t, config.Validate(), "must not contain a path")
	config.PublicURL = "http://cache.example.com"
	config.MaxSize = "0"
	require.ErrorContains(t, config.Validate(), "max_size must be positive")
	config.MaxSize = "10GiB"
	config.Upstreams["ghcr.io"] = configpkg.RuntimeCacheUpstreamConfig{Allow: []string{"*/invalid"}}
	require.ErrorContains(t, config.Validate(), "trailing wildcard")
	config.Upstreams["ghcr.io"] = configpkg.RuntimeCacheUpstreamConfig{Allow: []string{"devcontainers/*"}}
	config.Upstreams["GHCR.IO"] = configpkg.RuntimeCacheUpstreamConfig{}
	require.ErrorContains(t, config.Validate(), "lowercase registry host")
}
