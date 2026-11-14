// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"fmt"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	configpkg "gitea.dev/codespace/internal/config"
)

const (
	gatewayHTTPListen      = ":8080"
	gatewaySSHListen       = ":2222"
	cacheListen            = ":8443"
	cacheStoragePath       = "/var/lib/codespace-cache"
	gatewayHTTPPort        = int32(8080)
	gatewaySSHPort         = int32(2222)
	gatewayHTTPServicePort = int32(80)
	gatewaySSHServicePort  = int32(22)
	cacheContainerPort     = int32(8443)
	cacheServicePort       = int32(443)
)

func componentDuration(value, field string) (configpkg.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return configpkg.Duration(parsed), nil
}

func gatewayComponentConfig(component *api.CodespaceComponent) (configpkg.GatewayConfig, error) {
	if component.Spec.Role != "gateway" || component.Spec.Gateway == nil || component.Spec.Cache != nil {
		return configpkg.GatewayConfig{}, fmt.Errorf("component %s is not a Gateway", component.Name)
	}
	spec := component.Spec.Gateway
	handshakeTimeout, err := componentDuration(spec.SSH.HandshakeTimeout, "gateway SSH handshake timeout")
	if err != nil {
		return configpkg.GatewayConfig{}, err
	}
	failureWindow, err := componentDuration(spec.SSH.Auth.FailureWindow, "gateway SSH authentication failure window")
	if err != nil {
		return configpkg.GatewayConfig{}, err
	}
	ttl, err := componentDuration(spec.Sessions.TTL, "gateway session lifetime")
	if err != nil {
		return configpkg.GatewayConfig{}, err
	}
	idleTimeout, err := componentDuration(spec.Sessions.IdleTimeout, "gateway session idle timeout")
	if err != nil {
		return configpkg.GatewayConfig{}, err
	}
	revalidateInterval, err := componentDuration(spec.Sessions.RevalidateInterval, "gateway session revalidation interval")
	if err != nil {
		return configpkg.GatewayConfig{}, err
	}
	config := configpkg.GatewayConfig{
		HTTP: configpkg.GatewayHTTPConfig{Listen: gatewayHTTPListen, PublicURL: spec.EndpointBaseURL, TrustedProxyCIDRs: append([]string(nil), spec.TrustedProxyCIDRs...)},
		SSH: configpkg.GatewaySSHConfig{
			Listen: gatewaySSHListen, PublicAddr: spec.SSHExternalAddress, HandshakeTimeout: handshakeTimeout,
			MaxChannelsPerConnection: int(spec.SSH.MaxChannelsPerConnection),
			Auth: configpkg.GatewaySSHAuthConfig{
				MaxAttemptsPerIP: int(spec.SSH.Auth.MaxAttemptsPerIP), MaxAttemptsPerCodespace: int(spec.SSH.Auth.MaxAttemptsPerCodespace),
				MaxAttemptsPerIPCodespace: int(spec.SSH.Auth.MaxAttemptsPerIPCodespace), MaxAttemptsPerPublicKey: int(spec.SSH.Auth.MaxAttemptsPerPublicKey), FailureWindow: failureWindow,
			},
		},
		Sessions: configpkg.GatewaySessionConfig{
			TTL: ttl, IdleTimeout: idleTimeout, RevalidateInterval: revalidateInterval,
			MaxPerCodespace: int(spec.Sessions.MaxPerCodespace), MaxPerUser: int(spec.Sessions.MaxPerUser),
		},
		Limits: configpkg.GatewayLimitsConfig{
			MaxInflightTotal: int(spec.Limits.MaxInflightTotal), MaxInflightPerSession: int(spec.Limits.MaxInflightPerSession),
			PublicMaxConnectionsPerEndpoint: int(spec.Limits.PublicMaxConnectionsPerEndpoint), PublicMaxConnectionsPerIP: int(spec.Limits.PublicMaxConnectionsPerIP),
			ValidationMaxInflight: int(spec.Limits.ValidationMaxInflight),
		},
	}
	if err := config.Validate(); err != nil {
		return configpkg.GatewayConfig{}, fmt.Errorf("invalid Gateway configuration: %w", err)
	}
	return config, nil
}

func cacheComponentConfig(component *api.CodespaceComponent) (configpkg.CacheConfig, error) {
	if component.Spec.Role != "cache" || component.Spec.Cache == nil || component.Spec.Gateway != nil {
		return configpkg.CacheConfig{}, fmt.Errorf("component %s is not a Cache", component.Name)
	}
	config, err := component.Spec.Cache.RuntimeConfig()
	if err != nil {
		return configpkg.CacheConfig{}, err
	}
	config.ID, config.Name, config.Revision = component.Name, component.Spec.DisplayName, max(component.Generation, 1)
	config.Listen, config.Storage.Path = cacheListen, cacheStoragePath
	if err := config.Validate(); err != nil {
		return configpkg.CacheConfig{}, fmt.Errorf("invalid Cache configuration: %w", err)
	}
	return config, nil
}
