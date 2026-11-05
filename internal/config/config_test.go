// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config_test

import (
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/stretchr/testify/require"
)

func validGatewayConfig() configpkg.GatewayConfig {
	return configpkg.GatewayConfig{
		HTTP: configpkg.GatewayHTTPConfig{Listen: ":18081", PublicURL: "http://gateway.example.com:18081"},
		SSH: configpkg.GatewaySSHConfig{
			Listen: ":2222", PublicAddr: "gateway.example.com:22", HandshakeTimeout: configpkg.Duration(30 * time.Second), MaxChannelsPerConnection: 32,
			Auth: configpkg.GatewaySSHAuthConfig{MaxAttemptsPerIP: 30, MaxAttemptsPerCodespace: 20, MaxAttemptsPerIPCodespace: 10, MaxAttemptsPerPublicKey: 30, FailureWindow: configpkg.Duration(10 * time.Minute)},
		},
		Sessions: configpkg.GatewaySessionConfig{TTL: configpkg.Duration(8 * time.Hour), IdleTimeout: configpkg.Duration(30 * time.Minute), RevalidateInterval: configpkg.Duration(5 * time.Minute), MaxPerCodespace: 32, MaxPerUser: 128},
		Limits:   configpkg.GatewayLimitsConfig{MaxInflightTotal: 4096, MaxInflightPerSession: 32, PublicMaxConnectionsPerEndpoint: 64, PublicMaxConnectionsPerIP: 16, ValidationMaxInflight: 128},
	}
}

func TestGatewayConfigValidation(t *testing.T) {
	t.Parallel()

	config := validGatewayConfig()
	require.NoError(t, config.Validate())

	config.Limits.MaxInflightTotal = 4
	config.Limits.MaxInflightPerSession = 5
	require.ErrorContains(t, config.Validate(), "not exceed the total")

	config = validGatewayConfig()
	config.HTTP.PublicURL = "http://127.0.0.1:18081"
	require.ErrorContains(t, config.Validate(), "gateway.http.public_url")

	config = validGatewayConfig()
	config.SSH.HandshakeTimeout = configpkg.Duration(time.Millisecond)
	require.ErrorContains(t, config.Validate(), "handshake_timeout")
}

func TestCacheConfigValidation(t *testing.T) {
	t.Parallel()

	config := configpkg.CacheConfig{
		ID: "test", Name: "Test", Revision: 1, Enabled: true, Listen: "127.0.0.1:15000",
		Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: "cache-registry", MinFreeSpace: "1GiB"}, MaxSize: "10GiB",
		MaxAge: configpkg.Duration(24 * time.Hour), GCInterval: configpkg.Duration(time.Hour),
		Upstreams: map[string]configpkg.RuntimeCacheUpstreamConfig{"ghcr.io": {Allow: []string{"devcontainers/*"}}},
	}
	require.NoError(t, config.Validate())
	config.MaxAge = 0
	require.ErrorContains(t, config.Validate(), "retention durations must be positive")
	config.MaxAge = configpkg.Duration(24 * time.Hour)
	config.Storage.MinFreeSpace = ""
	require.ErrorContains(t, config.Validate(), "minimum free space are required")
	config.Storage.MinFreeSpace = "1GiB"
	config.Revision = 0
	require.ErrorContains(t, config.Validate(), "revision must be positive")
	config.Revision = 1

	config.MaxSize = "0"
	require.ErrorContains(t, config.Validate(), "max_size must be positive")
	config.MaxSize = "10GiB"
	config.Upstreams["ghcr.io"] = configpkg.RuntimeCacheUpstreamConfig{Allow: []string{"*/invalid"}}
	require.ErrorContains(t, config.Validate(), "trailing wildcard")
	config.Upstreams["ghcr.io"] = configpkg.RuntimeCacheUpstreamConfig{Allow: []string{"devcontainers/*"}}
	config.Upstreams["ghcr.io"] = configpkg.RuntimeCacheUpstreamConfig{}
	require.ErrorContains(t, config.Validate(), "at least one repository pattern")
	config.Upstreams["ghcr.io"] = configpkg.RuntimeCacheUpstreamConfig{Allow: []string{" devcontainers/*"}}
	require.ErrorContains(t, config.Validate(), "invalid pattern")
	config.Upstreams["ghcr.io"] = configpkg.RuntimeCacheUpstreamConfig{Allow: []string{"devcontainers/*"}}
	config.Upstreams["GHCR.IO"] = configpkg.RuntimeCacheUpstreamConfig{Allow: []string{"devcontainers/*"}}
	require.ErrorContains(t, config.Validate(), "lowercase registry host")
}
