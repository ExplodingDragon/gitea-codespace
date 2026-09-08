// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestGatewayRouteStoreKeepsSiteOwnership(t *testing.T) {
	t.Parallel()

	store := newGatewayRouteStore()
	first := gatewayEndpointRouteForTest("11111111-1111-4111-8111-111111111111", "web")
	first.siteID = 1
	second := gatewayEndpointRouteForTest("22222222-2222-4222-8222-222222222222", "web")
	second.siteID = 2
	second.instanceName = "runtime-2"
	for _, route := range []gatewayEndpointRoute{first, second} {
		if err := store.Put(route); err != nil {
			t.Fatalf("put route: %v", err)
		}
	}
	if siteID, ok := store.siteForCodespace(first.codespaceUUID); !ok || siteID != 1 {
		t.Fatalf("first site = %d, present = %v", siteID, ok)
	}
	if siteID, ok := store.siteForCodespace(second.codespaceUUID); !ok || siteID != 2 {
		t.Fatalf("second site = %d, present = %v", siteID, ok)
	}
}

func TestGatewayRouteStoreLoadsSharedGatewayRuntimes(t *testing.T) {
	store := newGatewayRouteStore()
	uuid := "11111111-1111-4111-8111-111111111111"
	err := store.ReplaceSharedGatewayRuntimes([]GatewayRuntimeSnapshot{{
		RuntimeUUID: uuid, SiteID: 2, InstanceName: "runtime-1", Workdir: "/workspaces/repo",
		UID: 1000, GID: 1000, ContainerID: "container-1", ContainerUser: "developer",
		ContainerWorkdir: "/workspaces/repo", EditorPort: 13337,
		Endpoints: []GatewayEndpointSnapshot{{EndpointID: "workspace", UpstreamPort: 13337}},
	}})
	if err != nil {
		t.Fatalf("replace shared runtimes: %v", err)
	}
	if siteID, ok := store.siteForCodespace(uuid); !ok || siteID != 2 {
		t.Fatalf("site = %d ok = %v", siteID, ok)
	}
	target, ok, err := store.LoadGatewayWorkspaceTarget(uuid)
	if err != nil || !ok || target.instanceName != "runtime-1" || target.containerID != "container-1" {
		t.Fatalf("target = %#v ok = %v err = %v", target, ok, err)
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

func TestGatewayTransportReusesConnectionsWithinRuntime(t *testing.T) {
	store := newGatewayRouteStore()
	t.Cleanup(store.Close)
	for siteID := int64(1); siteID <= 2; siteID++ {
		var connections atomic.Int32
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, siteID)
		}))
		server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				connections.Add(1)
			}
		}
		server.Start()
		t.Cleanup(server.Close)
		store.SetSiteBackend(siteID, &testWorkspaceCommandBackend{tcpAddress: strings.TrimPrefix(server.URL, "http://")})
		route := gatewayEndpointRouteForTest(fmt.Sprintf("runtime-%d", siteID), "web")
		route.siteID = siteID
		if err := store.Put(route); err != nil {
			t.Fatal(err)
		}
		transport, err := store.Transport(route)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: transport}
		for range 2 {
			response, err := client.Get("http://127.0.0.1:3000/")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(body) != fmt.Sprint(siteID) {
				t.Fatalf("body=%q err=%v", body, err)
			}
		}
		if connections.Load() != 1 {
			t.Fatalf("connections=%d", connections.Load())
		}
		route.upstreamPort++
		if err := store.Put(route); err != nil {
			t.Fatal(err)
		}
		next, err := store.Transport(route)
		if err != nil || next == transport {
			t.Fatalf("changed route retained transport: %v", err)
		}
	}
}
