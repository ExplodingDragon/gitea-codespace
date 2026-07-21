// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import "testing"
import "time"

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
}
