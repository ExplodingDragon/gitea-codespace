// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"github.com/stretchr/testify/require"
)

type testGatewaySessionRegistry struct {
	*sessionRegistry
}

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

func newGatewaySessionRegistry(t *testing.T) *testGatewaySessionRegistry {
	return newGatewaySessionRegistryFromConfig(t, validGatewayConfig())
}

func newGatewaySessionRegistryFromConfig(t *testing.T, config configpkg.GatewayConfig) *testGatewaySessionRegistry {
	return newGatewaySessionRegistryFromConfigAndKey(t, config, []byte("0123456789abcdef0123456789abcdef"))
}

func newGatewaySessionRegistryFromConfigAndKey(t *testing.T, config configpkg.GatewayConfig, key []byte) *testGatewaySessionRegistry {
	t.Helper()
	registry, err := newSessionRegistryFromConfig(config, key)
	require.NoError(t, err)
	return &testGatewaySessionRegistry{sessionRegistry: registry}
}

func (r *testGatewaySessionRegistry) Create(binding openTokenBinding, now time.Time) (string, error) {
	return r.CreateReplacingAny(binding, nil, now)
}

func (r *testGatewaySessionRegistry) CreateReplacing(binding openTokenBinding, replaceID string, now time.Time) (string, error) {
	return r.CreateReplacingAny(binding, []string{replaceID}, now)
}

func (r *testGatewaySessionRegistry) Authenticate(token, codespaceUUID, endpointID string, now time.Time) (gatewaySession, bool) {
	session, ok, ambiguous, err := r.ResolveAny(context.Background(), []string{token}, codespaceUUID, endpointID, now, nil)
	if err != nil || !ok || ambiguous {
		return gatewaySession{}, false
	}
	session, err = r.Accept(session, now)
	return session, err == nil
}

func gatewaySessionTestConfig(ttl, idleTimeout configpkg.Duration, maxPerCodespace, maxPerUser int) configpkg.GatewayConfig {
	if idleTimeout == 0 {
		idleTimeout = ttl
	}
	return configpkg.GatewayConfig{Sessions: configpkg.GatewaySessionConfig{
		TTL:             ttl,
		IdleTimeout:     idleTimeout,
		MaxPerCodespace: maxPerCodespace,
		MaxPerUser:      maxPerUser,
	}}
}

func TestGatewaySessionRegistryRejectsInvalidKey(t *testing.T) {
	_, err := newSessionRegistryFromConfig(validGatewayConfig(), make([]byte, 31))
	require.ErrorContains(t, err, "gateway session key must contain 32 bytes")
}

func TestGatewaySessionTicketMovesBetweenReplicas(t *testing.T) {
	config := gatewaySessionTestConfig(configpkg.Duration(time.Hour), configpkg.Duration(time.Minute), 10, 10)
	key := []byte("0123456789abcdef0123456789abcdef")
	first := newGatewaySessionRegistryFromConfigAndKey(t, config, key)
	second := newGatewaySessionRegistryFromConfigAndKey(t, config, key)
	now := time.Unix(100, 0)
	token, err := first.Create(openTokenBinding{SiteUID: "site", UserID: 42, CodespaceUUID: "codespace", EndpointID: "workspace"}, now)
	require.NoError(t, err)

	session, ok := second.Authenticate(token, "codespace", "workspace", now.Add(time.Second))
	require.True(t, ok)
	require.Equal(t, int64(42), session.userID)
	require.NotEqual(t, token, session.token)
	_, ok = second.Authenticate(session.token, "codespace", "workspace", now.Add(30*time.Second))
	require.True(t, ok)
	_, ok = second.Authenticate(token, "codespace", "other", now.Add(time.Second))
	require.False(t, ok)

	other := newGatewaySessionRegistryFromConfigAndKey(t, config, []byte("fedcba9876543210fedcba9876543210"))
	_, ok = other.Authenticate(token, "codespace", "workspace", now.Add(time.Second))
	require.False(t, ok)
	tamperAt := len(token) / 2
	tamperedByte := byte('A')
	if token[tamperAt] == tamperedByte {
		tamperedByte = 'B'
	}
	tampered := token[:tamperAt] + string(tamperedByte) + token[tamperAt+1:]
	_, ok = second.Authenticate(tampered, "codespace", "workspace", now.Add(time.Second))
	require.False(t, ok)
}

func TestGatewayActivityCountsOnlyAuthenticatedLiveConnections(t *testing.T) {
	registry := newGatewaySessionRegistry(t)
	const runtimeUUID = "11111111-1111-4111-8111-111111111111"
	sessionID, err := registry.Create(openTokenBinding{SiteUID: "site-1", UserID: 1, CodespaceUUID: runtimeUUID, EndpointID: "workspace"}, time.Now())
	require.NoError(t, err)
	require.Empty(t, registry.Activity(time.Now()).runtimes)
	session, ok := registry.Authenticate(sessionID, runtimeUUID, "workspace", time.Now())
	require.True(t, ok)

	releaseRequest := registry.BeginSessionCancelable(session.id, runtimeUUID, nil)
	requireSignal(t, registry.ActivityChanges())
	require.Equal(t, map[string]int64{runtimeUUID: 1}, registry.Activity(time.Now()).runtimes)
	releaseSecond := registry.BeginSessionCancelable(session.id, runtimeUUID, nil)
	requireNoSignal(t, registry.ActivityChanges())
	require.Equal(t, map[string]int64{runtimeUUID: 2}, registry.Activity(time.Now()).runtimes)
	releaseSecond()
	requireNoSignal(t, registry.ActivityChanges())
	require.Equal(t, map[string]int64{runtimeUUID: 1}, registry.Activity(time.Now()).runtimes)
	releaseRequest()
	requireSignal(t, registry.ActivityChanges())
	require.Empty(t, registry.Activity(time.Now()).runtimes)

	releaseSSH, ok := registry.BeginSSHSession("site-1", runtimeUUID, 1, nil, time.Now())
	require.True(t, ok)
	requireSignal(t, registry.ActivityChanges())
	require.Equal(t, map[string]int64{runtimeUUID: 1}, registry.Activity(time.Now()).runtimes)
	releaseSSH()
	requireSignal(t, registry.ActivityChanges())
	require.Empty(t, registry.Activity(time.Now()).runtimes)
}

func requireSignal(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
	default:
		t.Fatal("activity change was not signaled")
	}
}

func requireNoSignal(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
		t.Fatal("activity change was signaled without an active-state transition")
	default:
	}
}

func TestGatewaySessionRegistryAuthenticatesGatewaySession(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistry(t)
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	session, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now.Add(gatewaySessionConnectTimeout-time.Second))
	if !ok {
		t.Fatalf("session was not authenticated")
	}
	if session.userID != 42 || session.codespaceUUID != "codespace-1" || session.endpointID != "workspace" {
		t.Fatalf("session = %#v", session)
	}
	if _, ok := registry.Authenticate(sessionID, "codespace-1", "other", now); ok {
		t.Fatalf("session authenticated with wrong endpoint")
	}
}

func TestGatewaySessionRegistryDropsExpiredConnectingSession(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), 0, 1, 1))
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now.Add(gatewaySessionConnectTimeout+time.Nanosecond)); ok {
		t.Fatalf("expired connecting session authenticated")
	}
	if _, err := registry.Create(openTokenBinding{
		SiteUID: "site-1", UserID: 42, CodespaceUUID: "codespace-1", EndpointID: "workspace",
	}, now.Add(gatewaySessionConnectTimeout+time.Nanosecond)); err != nil {
		t.Fatalf("create session after connecting timeout: %v", err)
	}
}

func TestGatewaySessionRegistryLimitsEndpointAndSSHSessionTogether(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), 0, 1, 10))
	now := time.Unix(100, 0)
	if _, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now); err != nil {
		t.Fatalf("create endpoint session: %v", err)
	}
	if release, ok := registry.BeginSSHSession("site-1", "codespace-1", 42, nil, now); ok {
		release()
		t.Fatalf("expected ssh session to share codespace limit")
	}
}

func TestGatewaySessionRegistryReplacingSameBindingReleasesSessionLimit(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), 0, 1, 1))
	now := time.Unix(100, 0)
	firstID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	session, ok := registry.Authenticate(firstID, "codespace-1", "workspace", now)
	require.True(t, ok)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release := registry.BeginSessionCancelable(session.id, "codespace-1", cancel)
	defer release()
	secondID, err := registry.CreateReplacing(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, firstID, now)
	if err != nil {
		t.Fatalf("replace session: %v", err)
	}
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Empty(t, registry.Activity(now).runtimes)
	release()
	require.Equal(t, 1, registry.sessionCountByCodespace["codespace-1"])
	if _, ok := registry.Authenticate(firstID, "codespace-1", "workspace", now); ok {
		t.Fatalf("old session still authenticates")
	}
	if _, ok := registry.Authenticate(secondID, "codespace-1", "workspace", now); !ok {
		t.Fatalf("new session does not authenticate")
	}
	if _, err := registry.CreateReplacing(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "web",
	}, secondID, now); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("replace different endpoint err = %v", err)
	}
}

func TestGatewaySessionRegistryMatchesOnlyCurrentBindingCandidates(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), 0, 10, 10))
	now := time.Unix(100, 0)
	validID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create valid session: %v", err)
	}
	otherID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "web",
	}, now)
	if err != nil {
		t.Fatalf("create other binding session: %v", err)
	}
	session, ok, ambiguous, err := registry.ResolveAny(t.Context(), []string{"unknown", otherID, validID}, "codespace-1", "workspace", now, nil)
	require.NoError(t, err)
	if !ok || ambiguous {
		t.Fatalf("authenticate candidates ok=%v ambiguous=%v", ok, ambiguous)
	}
	if session.codespaceUUID != "codespace-1" || session.endpointID != "workspace" {
		t.Fatalf("matched session = %#v", session)
	}
}

func TestGatewaySessionRegistryRejectsMultipleCurrentBindingCandidates(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), 0, 10, 10))
	now := time.Unix(100, 0)
	firstID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	secondID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if _, ok, ambiguous, err := registry.ResolveAny(t.Context(), []string{firstID, secondID}, "codespace-1", "workspace", now, nil); err != nil || ok || !ambiguous {
		t.Fatalf("authenticate multiple current candidates ok=%v ambiguous=%v", ok, ambiguous)
	}
	if _, err := registry.CreateReplacingAny(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, []string{firstID, secondID}, now); !errors.Is(err, errGatewaySessionAmbiguous) {
		t.Fatalf("replace multiple current candidates err = %v", err)
	}
}

func TestGatewaySessionRegistryLimitsUserAcrossCodespaces(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), 0, 10, 1))
	now := time.Unix(100, 0)
	release, ok := registry.BeginSSHSession("site-1", "codespace-1", 42, nil, now)
	if !ok {
		t.Fatalf("begin ssh session")
	}
	defer release()
	if _, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-2",
		EndpointID:    "workspace",
	}, now); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("endpoint session over user limit err = %v", err)
	}
}

func TestGatewaySessionRegistrySeparatesUsersBySite(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), 0, 10, 1))
	now := time.Unix(100, 0)
	for _, binding := range []openTokenBinding{
		{SiteUID: "site-1", UserID: 42, CodespaceUUID: "codespace-1", EndpointID: "workspace"},
		{SiteUID: "site-2", UserID: 42, CodespaceUUID: "codespace-2", EndpointID: "workspace"},
	} {
		if _, err := registry.Create(binding, now); err != nil {
			t.Fatalf("create session for %s: %v", binding.SiteUID, err)
		}
	}
	if _, err := registry.Create(openTokenBinding{SiteUID: "site-1", UserID: 42, CodespaceUUID: "codespace-3", EndpointID: "workspace"}, now); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("same-site user session over limit err = %v", err)
	}
}

func TestGatewaySessionRegistryPortTTLReleasesSessionLimit(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Minute), 0, 1, 1))
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "app-3000",
	}, now)
	if err != nil {
		t.Fatalf("create endpoint session: %v", err)
	}
	if _, ok := registry.Authenticate(sessionID, "codespace-1", "app-3000", now); !ok {
		t.Fatalf("authenticate endpoint session")
	}
	if _, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now.Add(time.Minute-time.Nanosecond)); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("endpoint session at ttl boundary err = %v", err)
	}
	if _, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now.Add(time.Minute+time.Nanosecond)); err != nil {
		t.Fatalf("create endpoint session after ttl: %v", err)
	}
}

func TestGatewaySessionRegistryIdleTimeoutReleasesEndpointSession(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), configpkg.Duration(time.Minute), 1, 1))
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create endpoint session: %v", err)
	}
	if _, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now); !ok {
		t.Fatalf("authenticate endpoint session")
	}
	if _, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now.Add(time.Minute-time.Nanosecond)); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("endpoint session at idle boundary err = %v", err)
	}
	if _, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now.Add(time.Minute+time.Nanosecond)); err != nil {
		t.Fatalf("create endpoint session after idle timeout: %v", err)
	}
}

func TestGatewayWorkspaceSessionRenewsAcrossReplicas(t *testing.T) {
	t.Parallel()

	config := gatewaySessionTestConfig(configpkg.Duration(time.Hour), configpkg.Duration(time.Minute), 10, 10)
	registry := newGatewaySessionRegistryFromConfig(t, config)
	other := newGatewaySessionRegistryFromConfig(t, config)
	now := time.Now()
	started := now.Add(-9 * time.Hour)
	token, err := registry.Create(openTokenBinding{SiteUID: "site-1", UserID: 42, CodespaceUUID: "codespace-1", EndpointID: "workspace"}, started)
	require.NoError(t, err)
	session, ok := registry.Authenticate(token, "codespace-1", "workspace", started)
	require.True(t, ok)
	release := registry.BeginSessionCancelable(session.id, "codespace-1", nil)
	defer release()

	activity := registry.Activity(now)
	require.Equal(t, map[string]int64{"codespace-1": 1}, activity.runtimes)
	require.Len(t, activity.workspaces, 1)
	require.True(t, activity.workspaces[0].Active)
	registry.requireWorkspaceAck = true
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, registry.waitWorkspaceActivity(canceled, session.id), context.Canceled)
	registry.acknowledgeWorkspaceActivity(activity.revision)
	require.NoError(t, registry.waitWorkspaceActivity(t.Context(), session.id))

	lookup := func(_ context.Context, identity *componentv1.WorkspaceSessionIdentity) (bool, error) {
		require.Equal(t, activity.workspaces[0].Identity, identity)
		return true, nil
	}
	candidate, ok, ambiguous, err := other.ResolveAny(t.Context(), []string{session.token}, "codespace-1", "workspace", now, lookup)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, ambiguous)
	require.Empty(t, other.sessions, "identity resolution must not establish activity")
	renewed, err := other.Accept(candidate, now)
	require.NoError(t, err)
	require.Equal(t, session.id, renewed.id)
	require.Equal(t, now.Add(time.Hour), renewed.expires)
	_, ok = other.Authenticate(renewed.token, "codespace-1", "workspace", now.Add(time.Second))
	require.True(t, ok)

	release()
	lastActive := registry.sessions[session.id].lastActive
	require.WithinDuration(t, time.Now(), lastActive, time.Second)
	idle := registry.Activity(lastActive)
	require.Empty(t, idle.runtimes)
	require.Len(t, idle.workspaces, 1)
	require.False(t, idle.workspaces[0].Active)
	require.Equal(t, lastActive.UnixMilli(), idle.workspaces[0].LastActiveUnixMilliseconds)
	require.Greater(t, idle.revision, activity.revision)
	_, ok, _, err = registry.ResolveAny(t.Context(), []string{session.token}, "codespace-1", "workspace", lastActive.Add(30*time.Second), nil)
	require.NoError(t, err)
	require.True(t, ok, "closed long connection leaves a full idle window")
	_, ok, _, err = registry.ResolveAny(t.Context(), []string{session.token}, "codespace-1", "workspace", lastActive.Add(time.Minute), nil)
	require.NoError(t, err)
	require.False(t, ok)

	portToken, err := other.Create(openTokenBinding{SiteUID: "site-1", UserID: 42, CodespaceUUID: "codespace-1", EndpointID: "app-3000"}, now)
	require.NoError(t, err)
	port, ok := other.Authenticate(portToken, "codespace-1", "app-3000", now)
	require.True(t, ok)
	endPort := other.BeginSessionCancelable(port.id, "codespace-1", nil)
	defer endPort()
	_, ok, _, err = other.ResolveAny(t.Context(), []string{port.token}, "codespace-1", "app-3000", now.Add(2*time.Hour), lookup)
	require.NoError(t, err)
	require.False(t, ok, "port traffic does not use Workspace renewal")
	_, ok, _, err = other.ResolveAny(t.Context(), []string{renewed.token}, "codespace-1", "workspace", now.Add(2*time.Minute), nil)
	require.NoError(t, err)
	require.False(t, ok, "port activity does not keep the Workspace session alive")
}

func TestGatewaySessionRegistryAuthenticateRefreshesIdleTimeout(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), configpkg.Duration(time.Minute), 1, 1))
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(openTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create endpoint session: %v", err)
	}
	session, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now)
	if !ok {
		t.Fatalf("authenticate endpoint session")
	}
	session, ok = registry.Authenticate(session.token, "codespace-1", "workspace", now.Add(30*time.Second))
	if !ok {
		t.Fatalf("refresh endpoint session")
	}
	session, ok = registry.Authenticate(session.token, "codespace-1", "workspace", now.Add(89*time.Second))
	if !ok {
		t.Fatalf("session should still be active after refreshed idle timeout")
	}
	if _, ok := registry.Authenticate(session.token, "codespace-1", "workspace", now.Add(90*time.Second+time.Minute+time.Nanosecond)); ok {
		t.Fatalf("session authenticated after refreshed idle timeout expired")
	}
}
