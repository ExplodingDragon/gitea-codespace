// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayBrowserAuthOpenURLUsesCodespaceDetail(t *testing.T) {
	t.Parallel()

	routes := newRouteStore(nil)
	t.Cleanup(routes.Close)
	runtime := testGatewayRuntime()
	runtime.GiteaWebURL = "https://gitea.example.test/git/"
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{runtime}))
	auth := newBrowserAuth(routes)

	got, ok := auth.openURL("site-1/42", "app-3000")
	if !ok {
		t.Fatal("open URL is unavailable")
	}
	want := "https://gitea.example.test/git/-/codespaces/42?open_endpoint=app-3000"
	if got != want {
		t.Fatalf("open URL = %q, want %q", got, want)
	}
	otherID := "site-2/42"
	otherURL := "https://other.example.test/"
	other := runtime
	other.SiteUID, other.ResourceUID, other.RuntimeKey, other.GiteaWebURL = "site-2", "22222222-2222-4222-8222-222222222222", otherID, otherURL
	require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{runtime, other}))
	if target, ok := auth.openURL(otherID, "workspace"); !ok || !strings.HasPrefix(target, otherURL) {
		t.Fatal("browser recovery used the wrong Gitea site")
	}
	if _, ok := auth.openURL("site-3/42", "workspace"); ok {
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
