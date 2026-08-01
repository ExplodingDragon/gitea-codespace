// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newGatewaySessionRegistry() *gatewaySessionRegistry {
	return newGatewaySessionRegistryFromConfig(DefaultConfig().Gateway)
}

func gatewaySessionTestConfig(ttl, idleTimeout Duration, maxPerCodespace, maxPerUser int) GatewayConfig {
	return GatewayConfig{Sessions: GatewaySessionConfig{
		TTL:             ttl,
		IdleTimeout:     idleTimeout,
		MaxPerCodespace: maxPerCodespace,
		MaxPerUser:      maxPerUser,
	}}
}

func TestGatewaySessionRegistryTracksLiveSessions(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistry()
	endFirst := registry.Begin("codespace-1")
	endSecond := registry.Begin("codespace-1")
	registry.Begin("")()

	if live := registry.LiveSessions("codespace-1"); live != 2 {
		t.Fatalf("live sessions = %d", live)
	}
	if live := registry.LiveSessions(""); live != 0 {
		t.Fatalf("empty uuid live sessions = %d", live)
	}

	endFirst()
	endFirst()
	if live := registry.LiveSessions("codespace-1"); live != 1 {
		t.Fatalf("live sessions after first end = %d", live)
	}

	endSecond()
	if live := registry.LiveSessions("codespace-1"); live != 0 {
		t.Fatalf("live sessions after second end = %d", live)
	}
}

func TestGatewaySessionRegistryDeleteCodespaceCancelsLiveConnections(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	release := registry.BeginCancelable("codespace-1", cancel)
	if live := registry.LiveSessions("codespace-1"); live != 1 {
		t.Fatalf("live sessions before delete = %d", live)
	}
	registry.DeleteCodespace("codespace-1")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatalf("live connection was not canceled")
	}
	if live := registry.LiveSessions("codespace-1"); live != 0 {
		t.Fatalf("live sessions after delete = %d", live)
	}
	release()
}

func TestGatewaySessionRegistryAuthenticatesGatewaySession(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistry()
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if live := registry.liveSessionsAt("codespace-1", now); live != 1 {
		t.Fatalf("connecting live sessions = %d", live)
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

	registry := newGatewaySessionRegistry()
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now.Add(gatewaySessionConnectTimeout+time.Nanosecond)); ok {
		t.Fatalf("expired connecting session authenticated")
	}
	if live := registry.liveSessionsAt("codespace-1", now.Add(gatewaySessionConnectTimeout+time.Nanosecond)); live != 0 {
		t.Fatalf("live sessions after connecting timeout = %d", live)
	}
}

func TestGatewaySessionRegistryLimitsEndpointAndSSHSessionTogether(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(gatewaySessionTestConfig(Duration(time.Hour), 0, 1, 10))
	now := time.Unix(100, 0)
	if _, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now); err != nil {
		t.Fatalf("create endpoint session: %v", err)
	}
	if release, ok := registry.BeginSSHSession("codespace-1", 42, nil, now); ok {
		release()
		t.Fatalf("expected ssh session to share codespace limit")
	}
}

func TestGatewaySessionRegistryReplacingSameBindingReleasesSessionLimit(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(gatewaySessionTestConfig(Duration(time.Hour), 0, 1, 1))
	now := time.Unix(100, 0)
	firstID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	secondID, err := registry.CreateReplacing(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
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
	if _, err := registry.CreateReplacing(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "web",
	}, secondID, now); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("replace different endpoint err = %v", err)
	}
}

func TestGatewaySessionRegistryMatchesOnlyCurrentBindingCandidates(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(gatewaySessionTestConfig(Duration(time.Hour), 0, 10, 10))
	now := time.Unix(100, 0)
	validID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create valid session: %v", err)
	}
	otherID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "web",
	}, now)
	if err != nil {
		t.Fatalf("create other binding session: %v", err)
	}
	session, ok, ambiguous := registry.AuthenticateAny([]string{"unknown", otherID, validID}, "codespace-1", "workspace", now)
	if !ok || ambiguous {
		t.Fatalf("authenticate candidates ok=%v ambiguous=%v", ok, ambiguous)
	}
	if session.id != validID {
		t.Fatalf("matched session id = %q", session.id)
	}
}

func TestGatewaySessionRegistryRejectsMultipleCurrentBindingCandidates(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(gatewaySessionTestConfig(Duration(time.Hour), 0, 10, 10))
	now := time.Unix(100, 0)
	firstID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	secondID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if _, ok, ambiguous := registry.AuthenticateAny([]string{firstID, secondID}, "codespace-1", "workspace", now); ok || !ambiguous {
		t.Fatalf("authenticate multiple current candidates ok=%v ambiguous=%v", ok, ambiguous)
	}
	if _, err := registry.CreateReplacingAny(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, []string{firstID, secondID}, now); !errors.Is(err, errGatewaySessionAmbiguous) {
		t.Fatalf("replace multiple current candidates err = %v", err)
	}
}

func TestGatewaySessionRegistryLimitsUserAcrossCodespaces(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(gatewaySessionTestConfig(Duration(time.Hour), 0, 10, 1))
	now := time.Unix(100, 0)
	release, ok := registry.BeginSSHSession("codespace-1", 42, nil, now)
	if !ok {
		t.Fatalf("begin ssh session")
	}
	defer release()
	if _, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-2",
		endpointID:    "workspace",
	}, now); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("endpoint session over user limit err = %v", err)
	}
}

func TestGatewaySessionRegistryTTLReleasesEndpointSessionLimit(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(gatewaySessionTestConfig(Duration(time.Minute), 0, 1, 1))
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create endpoint session: %v", err)
	}
	if _, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now); !ok {
		t.Fatalf("authenticate endpoint session")
	}
	if _, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now.Add(time.Minute)); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("endpoint session at ttl boundary err = %v", err)
	}
	if _, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now.Add(time.Minute+time.Nanosecond)); err != nil {
		t.Fatalf("create endpoint session after ttl: %v", err)
	}
}

func TestGatewaySessionRegistryIdleTimeoutReleasesEndpointSession(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(gatewaySessionTestConfig(Duration(time.Hour), Duration(time.Minute), 1, 1))
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create endpoint session: %v", err)
	}
	if _, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now); !ok {
		t.Fatalf("authenticate endpoint session")
	}
	if _, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now.Add(time.Minute)); !errors.Is(err, errGatewaySessionLimitReached) {
		t.Fatalf("endpoint session at idle boundary err = %v", err)
	}
	if _, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now.Add(time.Minute+time.Nanosecond)); err != nil {
		t.Fatalf("create endpoint session after idle timeout: %v", err)
	}
	if live := registry.liveSessionsAt("codespace-1", now.Add(time.Minute+time.Nanosecond)); live != 1 {
		t.Fatalf("live sessions after idle replacement = %d", live)
	}
}

func TestGatewaySessionRegistryAuthenticateRefreshesIdleTimeout(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(gatewaySessionTestConfig(Duration(time.Hour), Duration(time.Minute), 1, 1))
	now := time.Unix(100, 0)
	sessionID, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-1",
		endpointID:    "workspace",
	}, now)
	if err != nil {
		t.Fatalf("create endpoint session: %v", err)
	}
	if _, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now); !ok {
		t.Fatalf("authenticate endpoint session")
	}
	if _, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now.Add(30*time.Second)); !ok {
		t.Fatalf("refresh endpoint session")
	}
	if _, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now.Add(90*time.Second)); !ok {
		t.Fatalf("session should still be active after refreshed idle timeout")
	}
	if _, ok := registry.Authenticate(sessionID, "codespace-1", "workspace", now.Add(90*time.Second+time.Minute+time.Nanosecond)); ok {
		t.Fatalf("session authenticated after refreshed idle timeout expired")
	}
}

func TestGatewaySessionRegistryDeleteCodespaceReleasesSSHSessionLimit(t *testing.T) {
	t.Parallel()

	registry := newGatewaySessionRegistryFromConfig(gatewaySessionTestConfig(Duration(time.Hour), 0, 10, 1))
	ctx, cancel := context.WithCancel(context.Background())
	release, ok := registry.BeginSSHSession("codespace-1", 42, cancel, time.Unix(100, 0))
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
	if _, err := registry.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: "codespace-2",
		endpointID:    "workspace",
	}, time.Unix(101, 0)); err != nil {
		t.Fatalf("create endpoint session after delete: %v", err)
	}
}
