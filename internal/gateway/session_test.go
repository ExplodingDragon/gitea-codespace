// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/stretchr/testify/require"
)

type testGatewaySessionRegistry struct {
	*SessionRegistry
}

func newGatewaySessionRegistry(t *testing.T) *testGatewaySessionRegistry {
	return newGatewaySessionRegistryFromConfig(t, configpkg.DefaultGatewayConfig())
}

func newGatewaySessionRegistryFromConfig(t *testing.T, config configpkg.GatewayConfig) *testGatewaySessionRegistry {
	return newGatewaySessionRegistryFromConfigAndKey(t, config, []byte("0123456789abcdef0123456789abcdef"))
}

func newGatewaySessionRegistryFromConfigAndKey(t *testing.T, config configpkg.GatewayConfig, key []byte) *testGatewaySessionRegistry {
	t.Helper()
	registry, err := NewSessionRegistryFromConfig(config, key)
	require.NoError(t, err)
	return &testGatewaySessionRegistry{SessionRegistry: registry}
}

func (r *testGatewaySessionRegistry) Create(binding OpenTokenBinding, now time.Time) (string, error) {
	return r.CreateReplacingAny(binding, nil, now)
}

func (r *testGatewaySessionRegistry) CreateReplacing(binding OpenTokenBinding, replaceID string, now time.Time) (string, error) {
	return r.CreateReplacingAny(binding, []string{replaceID}, now)
}

func (r *testGatewaySessionRegistry) Authenticate(token, codespaceUUID, endpointID string, now time.Time) (gatewaySession, bool) {
	session, ok, ambiguous := r.AuthenticateAny([]string{token}, codespaceUUID, endpointID, now)
	return session, ok && !ambiguous
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
	_, err := NewSessionRegistryFromConfig(configpkg.DefaultGatewayConfig(), make([]byte, 31))
	require.ErrorContains(t, err, "gateway session key must contain 32 bytes")
}

func TestGatewaySessionTicketMovesBetweenReplicas(t *testing.T) {
	config := gatewaySessionTestConfig(configpkg.Duration(time.Hour), configpkg.Duration(time.Minute), 10, 10)
	key := []byte("0123456789abcdef0123456789abcdef")
	first := newGatewaySessionRegistryFromConfigAndKey(t, config, key)
	second := newGatewaySessionRegistryFromConfigAndKey(t, config, key)
	now := time.Unix(100, 0)
	token, err := first.Create(OpenTokenBinding{SiteUID: "site", UserID: 42, CodespaceUUID: "codespace", EndpointID: "workspace"}, now)
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
	sessionID, err := registry.Create(OpenTokenBinding{SiteUID: "site-1", UserID: 1, CodespaceUUID: runtimeUUID, EndpointID: "workspace"}, time.Now())
	require.NoError(t, err)
	require.Empty(t, registry.Activity(time.Now()))
	session, ok := registry.Authenticate(sessionID, runtimeUUID, "workspace", time.Now())
	require.True(t, ok)

	releaseRequest := registry.BeginSessionCancelable(session.id, runtimeUUID, nil)
	requireSignal(t, registry.ActivityChanges())
	require.Equal(t, map[string]int{runtimeUUID: 1}, registry.Activity(time.Now()))
	releaseRequest()
	requireSignal(t, registry.ActivityChanges())
	require.Empty(t, registry.Activity(time.Now()))

	releaseSSH, ok := registry.BeginSSHSession("site-1", runtimeUUID, 1, nil, time.Now())
	require.True(t, ok)
	requireSignal(t, registry.ActivityChanges())
	require.Equal(t, map[string]int{runtimeUUID: 1}, registry.Activity(time.Now()))
	releaseSSH()
	requireSignal(t, registry.ActivityChanges())
	require.Empty(t, registry.Activity(time.Now()))
}

func requireSignal(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
	default:
		t.Fatal("activity change was not signaled")
	}
}

func TestGatewaySessionRegistryDeleteCodespaceCancelsLiveConnections(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistry(t)
	now := time.Now()
	token, err := registry.Create(OpenTokenBinding{SiteUID: "site-1", UserID: 1, CodespaceUUID: "codespace-1", EndpointID: "workspace"}, now)
	require.NoError(t, err)
	session, ok := registry.Authenticate(token, "codespace-1", "workspace", now)
	require.True(t, ok)
	ctx, cancel := context.WithCancel(context.Background())
	release := registry.BeginSessionCancelable(session.id, "codespace-1", cancel)
	require.Equal(t, map[string]int{"codespace-1": 1}, registry.Activity(now))
	registry.DeleteCodespace("codespace-1")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatalf("live connection was not canceled")
	}
	require.Empty(t, registry.Activity(now))
	release()
}

func TestGatewaySessionRegistryAuthenticatesGatewaySession(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistry(t)
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	session, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now.Add(gatewaySessionConnectTimeout))
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
	sessionID, err := registry.Create(OpenTokenBinding{
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
	if _, err := registry.Create(OpenTokenBinding{
		SiteUID: "site-1", UserID: 42, CodespaceUUID: "codespace-1", EndpointID: "workspace",
	}, now.Add(gatewaySessionConnectTimeout+time.Nanosecond)); err != nil {
		t.Fatalf("create session after connecting timeout: %v", err)
	}
}

func TestGatewaySessionRegistryLimitsEndpointAndSSHSessionTogether(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), 0, 1, 10))
	now := time.Unix(100, 0)
	if _, err := registry.Create(OpenTokenBinding{
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
	firstID, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	secondID, err := registry.CreateReplacing(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, firstID, now)
	if err != nil {
		t.Fatalf("replace session: %v", err)
	}
	if _, ok := registry.Authenticate(firstID, "codespace-1", "workspace", now); ok {
		t.Fatalf("old session still authenticates")
	}
	if _, ok := registry.Authenticate(secondID, "codespace-1", "workspace", now); !ok {
		t.Fatalf("new session does not authenticate")
	}
	if _, err := registry.CreateReplacing(OpenTokenBinding{
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
	validID, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create valid session: %v", err)
	}
	otherID, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "web",
	}, now)
	if err != nil {
		t.Fatalf("create other binding session: %v", err)
	}
	session, ok, ambiguous := registry.AuthenticateAny([]string{"unknown", otherID, validID}, "codespace-1", "workspace", now)
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
	firstID, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	secondID, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if _, ok, ambiguous := registry.AuthenticateAny([]string{firstID, secondID}, "codespace-1", "workspace", now); ok || !ambiguous {
		t.Fatalf("authenticate multiple current candidates ok=%v ambiguous=%v", ok, ambiguous)
	}
	if _, err := registry.CreateReplacingAny(OpenTokenBinding{
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
	if _, err := registry.Create(OpenTokenBinding{
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
	for _, binding := range []OpenTokenBinding{
		{SiteUID: "site-1", UserID: 42, CodespaceUUID: "codespace-1", EndpointID: "workspace"},
		{SiteUID: "site-2", UserID: 42, CodespaceUUID: "codespace-2", EndpointID: "workspace"},
	} {
		if _, err := registry.Create(binding, now); err != nil {
			t.Fatalf("create session for %s: %v", binding.SiteUID, err)
		}
	}
	if _, err := registry.Create(OpenTokenBinding{SiteUID: "site-1", UserID: 42, CodespaceUUID: "codespace-3", EndpointID: "workspace"}, now); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("same-site user session over limit err = %v", err)
	}
}

func TestGatewaySessionRegistryTTLReleasesEndpointSessionLimit(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Minute), 0, 1, 1))
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(OpenTokenBinding{
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
	if _, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now.Add(time.Minute)); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("endpoint session at ttl boundary err = %v", err)
	}
	if _, err := registry.Create(OpenTokenBinding{
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
	sessionID, err := registry.Create(OpenTokenBinding{
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
	if _, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now.Add(time.Minute)); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("endpoint session at idle boundary err = %v", err)
	}
	if _, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-1",
		EndpointID:    "workspace",
	}, now.Add(time.Minute+time.Nanosecond)); err != nil {
		t.Fatalf("create endpoint session after idle timeout: %v", err)
	}
}

func TestGatewaySessionRegistryAuthenticateRefreshesIdleTimeout(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), configpkg.Duration(time.Minute), 1, 1))
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(OpenTokenBinding{
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
	session, ok = registry.Authenticate(session.token, "codespace-1", "workspace", now.Add(90*time.Second))
	if !ok {
		t.Fatalf("session should still be active after refreshed idle timeout")
	}
	if _, ok := registry.Authenticate(session.token, "codespace-1", "workspace", now.Add(90*time.Second+time.Minute+time.Nanosecond)); ok {
		t.Fatalf("session authenticated after refreshed idle timeout expired")
	}
}

func TestGatewaySessionRegistryDeleteCodespaceReleasesSSHSessionLimit(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(t, gatewaySessionTestConfig(configpkg.Duration(time.Hour), 0, 10, 1))
	ctx, cancel := context.WithCancel(context.Background())
	release, ok := registry.BeginSSHSession("site-1", "codespace-1", 42, cancel, time.Unix(100, 0))
	if !ok {
		t.Fatalf("begin ssh session")
	}
	registry.DeleteCodespace("codespace-1")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatalf("ssh session was not canceled")
	}
	release()
	if _, err := registry.Create(OpenTokenBinding{
		SiteUID:       "site-1",
		UserID:        42,
		CodespaceUUID: "codespace-2",
		EndpointID:    "workspace",
	}, time.Unix(101, 0)); err != nil {
		t.Fatalf("create endpoint session after delete: %v", err)
	}
}
