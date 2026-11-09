// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"net/http/httptest"
	"testing"
)

func TestGatewayRouteMetadataUpdateKeepsConnections(t *testing.T) {
	t.Parallel()

	routes := newRouteStore(gatewayTestBackend{})
	t.Cleanup(routes.Close)
	route := endpointRoute{
		SiteUID: "site-1", GiteaWebURL: "https://old.example.test", PodUID: "pod-1", AgentAddress: "agent-1",
		CodespaceUUID: "codespace-1", EndpointID: "app", Label: "App", Public: true,
	}
	if err := routes.Put(route); err != nil {
		t.Fatal(err)
	}
	_, request, release, ok := routes.BeginProxy(httptest.NewRequest("GET", "http://gateway.example.test", nil), route.CodespaceUUID, route.EndpointID)
	if !ok {
		t.Fatal("begin proxy")
	}
	defer release()
	route.GiteaWebURL = "https://new.example.test"
	if err := routes.Put(route); err != nil {
		t.Fatal(err)
	}
	select {
	case <-request.Context().Done():
		t.Fatal("metadata-only route update canceled the proxy")
	default:
	}

	transport, err := routes.Transport(route)
	if err != nil {
		t.Fatal(err)
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("response header timeout = %s", transport.ResponseHeaderTimeout)
	}
	route.PodUID = "pod-2"
	if err := routes.Put(route); err != nil {
		t.Fatal(err)
	}
	select {
	case <-request.Context().Done():
	default:
		t.Fatal("target route update did not cancel the proxy")
	}
}
