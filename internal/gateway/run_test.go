// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"github.com/stretchr/testify/require"
)

func TestGatewayControlDeadlinePreservesActivePermit(t *testing.T) {
	now := time.Unix(100, 0)
	permitUntil := now.Add(5 * time.Second)

	require.Equal(t, permitUntil, gatewayControlDeadline(true, permitUntil, now))
	require.Equal(t, now.Add(gatewayInitialControlResponseTimeout), gatewayControlDeadline(false, permitUntil, now))
	require.Equal(t, now.Add(gatewayInitialControlResponseTimeout), gatewayControlDeadline(true, time.Time{}, now))
}

func TestGatewayConfigurationSnapshotIsComplete(t *testing.T) {
	want := configpkg.DefaultGatewayConfig()
	want.HTTP.Listen = ":8080"
	want.HTTP.PublicURL = "https://workspace.example.com"
	want.HTTP.TrustedProxyCIDRs = []string{"10.0.0.0/8", "2001:db8::/32"}
	want.SSH.Listen = ":2222"
	want.SSH.PublicAddr = "workspace.example.com:22"

	snapshot := &componentv1.GatewayConfigurationSnapshot{
		HttpListen: want.HTTP.Listen, PublicUrl: want.HTTP.PublicURL,
		SshListen: want.SSH.Listen, SshPublicAddress: want.SSH.PublicAddr,
		TrustedProxyCidrs:                append([]string(nil), want.HTTP.TrustedProxyCIDRs...),
		SessionTtlMilliseconds:           want.Sessions.TTL.ToStdlib().Milliseconds(),
		SessionIdleTimeoutMilliseconds:   want.Sessions.IdleTimeout.ToStdlib().Milliseconds(),
		RevalidateIntervalMilliseconds:   want.Sessions.RevalidateInterval.ToStdlib().Milliseconds(),
		MaxSessionsPerCodespace:          int32(want.Sessions.MaxPerCodespace),
		MaxSessionsPerUser:               int32(want.Sessions.MaxPerUser),
		MaxInflight:                      int32(want.Limits.MaxInflightTotal),
		MaxInflightPerSession:            int32(want.Limits.MaxInflightPerSession),
		MaxChannelsPerSshConnection:      int32(want.SSH.MaxChannelsPerConnection),
		SshHandshakeTimeoutMilliseconds:  want.SSH.HandshakeTimeout.ToStdlib().Milliseconds(),
		SshAuthMaxAttemptsPerIp:          int32(want.SSH.Auth.MaxAttemptsPerIP),
		SshAuthMaxAttemptsPerCodespace:   int32(want.SSH.Auth.MaxAttemptsPerCodespace),
		SshAuthMaxAttemptsPerIpCodespace: int32(want.SSH.Auth.MaxAttemptsPerIPCodespace),
		SshAuthMaxAttemptsPerPublicKey:   int32(want.SSH.Auth.MaxAttemptsPerPublicKey),
		SshAuthFailureWindowMilliseconds: want.SSH.Auth.FailureWindow.ToStdlib().Milliseconds(),
		PublicMaxConnectionsPerEndpoint:  int32(want.Limits.PublicMaxConnectionsPerEndpoint),
		PublicMaxConnectionsPerIp:        int32(want.Limits.PublicMaxConnectionsPerIP),
		ValidationMaxInflight:            int32(want.Limits.ValidationMaxInflight),
	}

	got, err := gatewayConfig(snapshot)
	require.NoError(t, err)
	require.Equal(t, want, got)
	_, err = gatewayConfig(&componentv1.GatewayConfigurationSnapshot{})
	require.ErrorContains(t, err, "invalid Gateway configuration snapshot")
}
