// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"strings"
	"testing"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

func newTestGatewayBrowserAuth(t *testing.T, address string) *BrowserAuth {
	t.Helper()
	routes := NewRouteStore(nil)
	t.Cleanup(routes.Close)
	if err := routes.Put(EndpointRoute{GiteaWebURL: address, CodespaceUUID: "11111111-1111-4111-8111-111111111111", EndpointID: runtimeendpoint.WorkspaceEndpointID, Label: runtimeendpoint.WorkspaceEndpointLabel}); err != nil {
		t.Fatal(err)
	}
	return NewBrowserAuth(routes)
}

func TestGatewayBrowserAuthOpenURLUsesCodespaceDetail(t *testing.T) {
	t.Parallel()

	auth := newTestGatewayBrowserAuth(t, "https://gitea.example.test/git/")

	got, ok := auth.openURL("11111111-1111-4111-8111-111111111111", "app-3000")
	if !ok {
		t.Fatal("open URL is unavailable")
	}
	want := "https://gitea.example.test/git/-/codespaces/11111111-1111-4111-8111-111111111111?open_endpoint=app-3000"
	if got != want {
		t.Fatalf("open URL = %q, want %q", got, want)
	}
	otherID := "22222222-2222-4222-8222-222222222222"
	otherURL := "https://other.example.test/"
	if err := auth.routes.Put(EndpointRoute{GiteaWebURL: otherURL, CodespaceUUID: otherID, EndpointID: runtimeendpoint.WorkspaceEndpointID, Label: runtimeendpoint.WorkspaceEndpointLabel}); err != nil {
		t.Fatal(err)
	}
	if target, ok := auth.openURL(otherID, "workspace"); !ok || !strings.HasPrefix(target, otherURL) {
		t.Fatal("browser recovery used the wrong Gitea site")
	}
	if _, ok := auth.openURL("33333333-3333-4333-8333-333333333333", "workspace"); ok {
		t.Fatal("browser recovery redirected an unknown runtime")
	}
}

func TestSanitizeGatewayReturnTo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "path and query", in: "/w/?folder=%2Fworkspace", want: "/w/?folder=%2Fworkspace"},
		{name: "external url", in: "https://example.test/w/", want: "/"},
		{name: "network path", in: "//example.test/w/", want: "/"},
		{name: "backslash", in: `/w\path`, want: "/"},
		{name: "reserved root path", in: "/.gitea-codespace/open", want: "/"},
		{name: "reserved nested path", in: "/w/.gitea-codespace/open", want: "/"},
		{name: "control character", in: "/w/\x1f", want: "/"},
		{name: "too long", in: "/" + strings.Repeat("a", gatewayReturnToMaxBytes), want: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeGatewayReturnTo(tt.in); got != tt.want {
				t.Fatalf("sanitize return_to = %q, want %q", got, tt.want)
			}
		})
	}
}
