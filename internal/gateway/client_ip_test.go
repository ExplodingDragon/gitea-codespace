// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayClientIPMiddleware(t *testing.T) {
	tests := []struct {
		name       string
		remote     string
		forwarded  string
		trusted    []string
		expectedIP string
	}{
		{name: "direct", remote: "198.51.100.10:1234", forwarded: "203.0.113.8", expectedIP: "198.51.100.10"},
		{name: "trusted proxy", remote: "10.0.0.2:1234", forwarded: "198.51.100.10, 10.0.0.3", trusted: []string{"10.0.0.0/8"}, expectedIP: "198.51.100.10"},
		{name: "trusted IPv6 proxy", remote: "[fd00::2]:1234", forwarded: "2001:db8::10, fd00::3", trusted: []string{"fd00::/8"}, expectedIP: "2001:db8::10"},
		{name: "malformed chain", remote: "10.0.0.2:1234", forwarded: "invalid", trusted: []string{"10.0.0.0/8"}, expectedIP: "10.0.0.2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://gateway.example", nil)
			request.RemoteAddr = test.remote
			request.Header.Set("X-Forwarded-For", test.forwarded)
			var actual string
			handler := gatewayClientIPMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				actual = gatewayPeerIP(request)
			}), test.trusted)
			handler.ServeHTTP(httptest.NewRecorder(), request)
			require.Equal(t, test.expectedIP, actual)
		})
	}
}
