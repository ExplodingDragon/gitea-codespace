// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGatewayAuthenticatedSourceUsesGatewayURLSchemeAndPort(t *testing.T) {
	t.Parallel()

	policy, err := newGatewayOriginPolicy("https://gateway.example.test")
	if err != nil {
		t.Fatalf("gateway origin policy: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://workspace.gateway.example.test/w/codespace/", nil)
	request.Header.Set("Origin", "https://workspace.gateway.example.test")
	if !isGatewayAuthenticatedSourceAllowed(request, policy) {
		t.Fatalf("expected https gateway origin to be allowed")
	}

	request.Header.Set("Origin", "http://workspace.gateway.example.test")
	if isGatewayAuthenticatedSourceAllowed(request, policy) {
		t.Fatalf("http origin should not match https gateway origin")
	}
}

func TestGatewayAuthenticatedSourceRequiresGatewayURLPort(t *testing.T) {
	t.Parallel()

	policy, err := newGatewayOriginPolicy("http://gateway.example.test:18081")
	if err != nil {
		t.Fatalf("gateway origin policy: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://workspace.gateway.example.test/w/codespace/", nil)
	request.Header.Set("Origin", "http://workspace.gateway.example.test:18081")
	request.Header.Set("X-Forwarded-Host", "workspace.gateway.example.test:18081")
	request.Header.Set("X-Forwarded-Proto", "http")
	if isGatewayAuthenticatedSourceAllowed(request, policy) {
		t.Fatalf("host without gateway_url port should be rejected")
	}

	request = httptest.NewRequest(http.MethodGet, "http://workspace.gateway.example.test:18081/w/codespace/", nil)
	request.Header.Set("Origin", "http://workspace.gateway.example.test:18081")
	if !isGatewayAuthenticatedSourceAllowed(request, policy) {
		t.Fatalf("host with gateway_url port should be allowed")
	}
}

func TestGatewayOriginPolicyParsesEndpointHosts(t *testing.T) {
	t.Parallel()

	policy, err := newGatewayOriginPolicy("https://gateway.example.test")
	if err != nil {
		t.Fatalf("gateway origin policy: %v", err)
	}
	tests := []struct {
		name          string
		host          string
		codespaceUUID string
		endpointID    string
		ok            bool
	}{
		{
			name:          "workspace",
			host:          "11111111111141118111111111111111.gateway.example.test",
			codespaceUUID: "11111111-1111-4111-8111-111111111111",
			endpointID:    "workspace",
			ok:            true,
		},
		{
			name:          "normal endpoint",
			host:          "app-3000-11111111111141118111111111111111.gateway.example.test",
			codespaceUUID: "11111111-1111-4111-8111-111111111111",
			endpointID:    "app-3000",
			ok:            true,
		},
		{
			name:          "endpoint with repeated hyphen",
			host:          "my-app-11111111111141118111111111111111.gateway.example.test",
			codespaceUUID: "11111111-1111-4111-8111-111111111111",
			endpointID:    "my-app",
			ok:            true,
		},
		{
			name: "workspace alias is not accepted",
			host: "workspace-11111111111141118111111111111111.gateway.example.test",
		},
		{
			name: "short uuid is not accepted",
			host: "app-3000-11111111.gateway.example.test",
		},
		{
			name: "multi label endpoint is not accepted",
			host: "app.11111111111141118111111111111111.gateway.example.test",
		},
		{
			name: "base domain is not an endpoint",
			host: "gateway.example.test",
		},
		{
			name: "trailing dot is not accepted",
			host: "11111111111141118111111111111111.gateway.example.test.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := policy.parseEndpointHost(test.host)
			if ok != test.ok {
				t.Fatalf("ok = %v, want %v", ok, test.ok)
			}
			if !ok {
				return
			}
			if got.codespaceUUID != test.codespaceUUID || got.endpointID != test.endpointID {
				t.Fatalf("endpoint host = %#v", got)
			}
		})
	}
}

func TestGatewayOriginPolicyParsesEndpointHostPort(t *testing.T) {
	t.Parallel()

	policy, err := newGatewayOriginPolicy("http://gateway.example.test:18081")
	if err != nil {
		t.Fatalf("gateway origin policy: %v", err)
	}
	if _, ok := policy.parseEndpointHost("11111111111141118111111111111111.gateway.example.test"); ok {
		t.Fatalf("host without gateway_url port should be rejected")
	}
	got, ok := policy.parseEndpointHost("11111111111141118111111111111111.gateway.example.test:18081")
	if !ok {
		t.Fatalf("host with gateway_url port should be accepted")
	}
	if got.codespaceUUID != "11111111-1111-4111-8111-111111111111" || got.endpointID != "workspace" {
		t.Fatalf("endpoint host = %#v", got)
	}
}
