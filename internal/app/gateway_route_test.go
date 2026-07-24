// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGatewayRouteStoreKeepsLeasesForLabelOnlyUpdate(t *testing.T) {
	t.Parallel()

	store := newGatewayRouteStore()
	route := gatewayEndpointRoute{
		codespaceUUID:  "11111111-1111-4111-8111-111111111111",
		endpointID:     "web",
		label:          "Web",
		upstreamScheme: "http",
		upstreamHost:   "10.0.0.12:3000",
		public:         true,
	}
	if err := store.Put(route); err != nil {
		t.Fatalf("put route: %v", err)
	}
	_, request, release, ok := store.BeginProxy(httptest.NewRequest("GET", "/p/", nil), route.codespaceUUID, route.endpointID)
	if !ok {
		t.Fatalf("begin proxy route failed")
	}
	defer release()

	route.label = "Web UI"
	if err := store.Put(route); err != nil {
		t.Fatalf("put label-only route: %v", err)
	}
	select {
	case <-request.Context().Done():
		t.Fatalf("label-only route update cancelled proxy")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestGatewayRouteStoreCancelsLeasesForRoutingUpdate(t *testing.T) {
	t.Parallel()

	store := newGatewayRouteStore()
	route := gatewayEndpointRoute{
		codespaceUUID:  "11111111-1111-4111-8111-111111111111",
		endpointID:     "web",
		label:          "Web",
		upstreamScheme: "http",
		upstreamHost:   "10.0.0.12:3000",
		public:         true,
	}
	if err := store.Put(route); err != nil {
		t.Fatalf("put route: %v", err)
	}
	_, request, release, ok := store.BeginProxy(httptest.NewRequest("GET", "/p/", nil), route.codespaceUUID, route.endpointID)
	if !ok {
		t.Fatalf("begin proxy route failed")
	}
	defer release()

	route.upstreamHost = "10.0.0.12:3001"
	if err := store.Put(route); err != nil {
		t.Fatalf("put routing update: %v", err)
	}
	assertGatewayRouteProxyCancelled(t, request)
}

func TestGatewayRouteStoreDeletesEndpointSessionsForRoutingUpdate(t *testing.T) {
	t.Parallel()

	store := newGatewayRouteStore()
	sessions := newGatewaySessionRegistry()
	store.SetSessionRegistry(sessions)
	route := gatewayEndpointRoute{
		codespaceUUID:  "11111111-1111-4111-8111-111111111111",
		endpointID:     "web",
		label:          "Web",
		upstreamScheme: "http",
		upstreamHost:   "10.0.0.12:3000",
		public:         false,
	}
	if err := store.Put(route); err != nil {
		t.Fatalf("put route: %v", err)
	}
	sessionID, err := sessions.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: route.codespaceUUID,
		endpointID:    route.endpointID,
	}, time.Now())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, ok := sessions.Authenticate(sessionID, route.codespaceUUID, route.endpointID, time.Now()); !ok {
		t.Fatalf("session did not authenticate before route update")
	}

	route.public = true
	if err := store.Put(route); err != nil {
		t.Fatalf("put route access update: %v", err)
	}
	if _, ok := sessions.Authenticate(sessionID, route.codespaceUUID, route.endpointID, time.Now()); ok {
		t.Fatalf("session authenticated after route update")
	}
}

func TestGatewayRouteStoreCancelsLeasesForDelete(t *testing.T) {
	t.Parallel()

	store := newGatewayRouteStore()
	route := gatewayEndpointRoute{
		codespaceUUID:  "11111111-1111-4111-8111-111111111111",
		endpointID:     "web",
		label:          "Web",
		upstreamScheme: "http",
		upstreamHost:   "10.0.0.12:3000",
		public:         true,
	}
	if err := store.Put(route); err != nil {
		t.Fatalf("put route: %v", err)
	}
	_, request, release, ok := store.BeginProxy(httptest.NewRequest("GET", "/p/", nil), route.codespaceUUID, route.endpointID)
	if !ok {
		t.Fatalf("begin proxy route failed")
	}
	defer release()

	store.Delete(route.codespaceUUID, route.endpointID)
	assertGatewayRouteProxyCancelled(t, request)
}

func TestGatewayRouteStoreCloseCodespaceAccessCancelsLeasesAndSessions(t *testing.T) {
	t.Parallel()

	store := newGatewayRouteStore()
	sessions := newGatewaySessionRegistry()
	store.SetSessionRegistry(sessions)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	otherUUID := "22222222-2222-4222-8222-222222222222"
	for _, route := range []gatewayEndpointRoute{
		{
			codespaceUUID:  codespaceUUID,
			endpointID:     "web",
			label:          "Web",
			upstreamScheme: "http",
			upstreamHost:   "10.0.0.12:3000",
			public:         true,
		},
		{
			codespaceUUID:  otherUUID,
			endpointID:     "web",
			label:          "Web",
			upstreamScheme: "http",
			upstreamHost:   "10.0.0.13:3000",
			public:         true,
		},
	} {
		if err := store.Put(route); err != nil {
			t.Fatalf("put route: %v", err)
		}
	}
	_, request, release, ok := store.BeginProxy(httptest.NewRequest("GET", "/p/", nil), codespaceUUID, "web")
	if !ok {
		t.Fatalf("begin proxy route failed")
	}
	defer release()
	_, otherRequest, otherRelease, ok := store.BeginProxy(httptest.NewRequest("GET", "/p/", nil), otherUUID, "web")
	if !ok {
		t.Fatalf("begin other proxy route failed")
	}
	defer otherRelease()
	sessionID, err := sessions.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: codespaceUUID,
		endpointID:    "web",
	}, time.Now())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	store.CloseCodespaceAccess(codespaceUUID)
	assertGatewayRouteProxyCancelled(t, request)
	select {
	case <-otherRequest.Context().Done():
		t.Fatalf("other codespace proxy was cancelled")
	case <-time.After(10 * time.Millisecond):
	}
	if _, ok := sessions.Authenticate(sessionID, codespaceUUID, "web", time.Now()); ok {
		t.Fatalf("session authenticated after codespace access close")
	}
}

func assertGatewayRouteProxyCancelled(t *testing.T, request *http.Request) {
	t.Helper()

	select {
	case <-request.Context().Done():
	case <-time.After(time.Second):
		t.Fatalf("proxy route context was not cancelled")
	}
}
