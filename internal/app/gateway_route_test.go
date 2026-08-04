// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

func TestGatewayRouteStoreKeepsLeasesForLabelOnlyUpdate(t *testing.T) {
	t.Parallel()

	store := newGatewayRouteStore()
	route := gatewayEndpointRouteForTest("11111111-1111-4111-8111-111111111111", "web")
	route.public = true
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
	route := gatewayEndpointRouteForTest("11111111-1111-4111-8111-111111111111", "web")
	route.public = true
	if err := store.Put(route); err != nil {
		t.Fatalf("put route: %v", err)
	}
	_, request, release, ok := store.BeginProxy(httptest.NewRequest("GET", "/p/", nil), route.codespaceUUID, route.endpointID)
	if !ok {
		t.Fatalf("begin proxy route failed")
	}
	defer release()

	route.upstreamPort = 3001
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
	route := gatewayEndpointRouteForTest("11111111-1111-4111-8111-111111111111", "web")
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
	route := gatewayEndpointRouteForTest("11111111-1111-4111-8111-111111111111", "web")
	route.public = true
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

func TestGatewayRouteStoreClosesWorkspaceEndpointLease(t *testing.T) {
	t.Parallel()

	store := newGatewayRouteStore()
	sessions := newGatewaySessionRegistry()
	store.SetSessionRegistry(sessions)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	if err := store.Put(gatewayEndpointRoute{
		codespaceUUID: codespaceUUID,
		endpointID:    runtimeendpoint.WorkspaceEndpointID,
		label:         runtimeendpoint.WorkspaceEndpointLabel,
		instanceName:  "runtime-1",
		upstreamPort:  runtimeendpoint.WorkspaceEndpointPort,
	}); err != nil {
		t.Fatalf("put workspace endpoint: %v", err)
	}
	_, request, release, ok := store.BeginProxy(httptest.NewRequest("GET", "/w/", nil), codespaceUUID, runtimeendpoint.WorkspaceEndpointID)
	if !ok {
		t.Fatalf("begin workspace endpoint failed")
	}
	defer release()
	sessionID, err := sessions.Create(gatewayOpenTokenBinding{
		userID:        42,
		codespaceUUID: codespaceUUID,
		endpointID:    runtimeendpoint.WorkspaceEndpointID,
	}, time.Now())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, ok := sessions.Authenticate(sessionID, codespaceUUID, runtimeendpoint.WorkspaceEndpointID, time.Now()); !ok {
		t.Fatalf("session did not authenticate before access close")
	}

	store.CloseCodespaceAccess(codespaceUUID)
	assertGatewayRouteProxyCancelled(t, request)
	if _, ok := sessions.Authenticate(sessionID, codespaceUUID, runtimeendpoint.WorkspaceEndpointID, time.Now()); ok {
		t.Fatalf("session authenticated after access close")
	}
}

func TestGatewayRouteStoreRejectsInvalidWorkspaceEndpoint(t *testing.T) {
	t.Parallel()

	err := newGatewayRouteStore().Put(gatewayEndpointRoute{
		codespaceUUID: "11111111-1111-4111-8111-111111111111",
		endpointID:    runtimeendpoint.WorkspaceEndpointID,
		label:         runtimeendpoint.WorkspaceEndpointLabel,
		instanceName:  "runtime-1",
		upstreamPort:  runtimeendpoint.WorkspaceEndpointPort,
		public:        true,
	})
	if err == nil {
		t.Fatalf("public workspace endpoint route was accepted")
	}
}

func TestGatewayRouteStoreCloseCodespaceAccessCancelsLeasesAndSessions(t *testing.T) {
	t.Parallel()

	store := newGatewayRouteStore()
	sessions := newGatewaySessionRegistry()
	store.SetSessionRegistry(sessions)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	otherUUID := "22222222-2222-4222-8222-222222222222"
	for _, route := range []gatewayEndpointRoute{
		gatewayEndpointRouteForTest(codespaceUUID, "web"),
		gatewayEndpointRouteForTest(otherUUID, "web"),
	} {
		route.public = true
		if route.codespaceUUID == otherUUID {
			route.instanceName = "runtime-2"
		}
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

func gatewayEndpointRouteForTest(codespaceUUID, endpointID string) gatewayEndpointRoute {
	return gatewayEndpointRoute{
		codespaceUUID: codespaceUUID,
		endpointID:    endpointID,
		label:         "Web",
		instanceName:  "runtime-1",
		upstreamPort:  3000,
	}
}
