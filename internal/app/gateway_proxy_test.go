// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGatewayProxyRequestInjectsAuthenticatedContext(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "https://11111111-1111-4111-8111-111111111111.gateway.example.test/work", nil)
	request.RemoteAddr = "198.51.100.44:3000"
	request.Header.Set(gatewayProxyHeaderCodespaceUUID, "spoofed")
	request.Header.Set(gatewayProxyHeaderEndpointID, "spoofed")
	request.Header.Set(gatewayProxyHeaderAccess, "public")
	request.Header.Set(gatewayProxyHeaderUserID, "7")
	request.Header.Set("Forwarded", "for=203.0.113.1")
	request.Header.Set("X-Forwarded-For", "203.0.113.2")
	request.Header.Set("X-Forwarded-Proto", "http")
	request.Header.Set("X-Forwarded-Host", "evil.example.test")

	prepareGatewayProxyRequest(request, gatewayProxyRequestContext{
		codespaceUUID:  "11111111-1111-4111-8111-111111111111",
		endpointID:     "workspace",
		access:         "authenticated",
		userID:         42,
		externalScheme: "https",
	})
	if got := request.Header.Get(gatewayProxyHeaderCodespaceUUID); got != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("codespace uuid header = %q", got)
	}
	if got := request.Header.Get(gatewayProxyHeaderEndpointID); got != "workspace" {
		t.Fatalf("endpoint id header = %q", got)
	}
	if got := request.Header.Get(gatewayProxyHeaderAccess); got != "authenticated" {
		t.Fatalf("access header = %q", got)
	}
	if got := request.Header.Get(gatewayProxyHeaderUserID); got != "42" {
		t.Fatalf("user id header = %q", got)
	}
	if got := request.Header.Get("Forwarded"); got != "" {
		t.Fatalf("forwarded header = %q", got)
	}
	if got := request.Header.Get("X-Forwarded-For"); got != "198.51.100.44" {
		t.Fatalf("x-forwarded-for = %q", got)
	}
	if got := request.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("x-forwarded-proto = %q", got)
	}
	if got := request.Header.Get("X-Forwarded-Host"); got != "11111111-1111-4111-8111-111111111111.gateway.example.test" {
		t.Fatalf("x-forwarded-host = %q", got)
	}
}

func TestGatewayProxyRequestInjectsPublicContextWithoutUser(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "http://codespace.gateway.example.test/", nil)
	request.RemoteAddr = "198.51.100.45"
	request.Header.Set(gatewayProxyHeaderUserID, "42")

	prepareGatewayProxyRequest(request, gatewayProxyRequestContext{
		codespaceUUID:  "codespace-uuid",
		endpointID:     "web",
		access:         "public",
		externalScheme: "http",
		externalHost:   "web.codespace.gateway.example.test",
	})
	if got := request.Header.Get(gatewayProxyHeaderAccess); got != "public" {
		t.Fatalf("access header = %q", got)
	}
	if got := request.Header.Get(gatewayProxyHeaderUserID); got != "" {
		t.Fatalf("user id header = %q", got)
	}
	if got := request.Header.Get("X-Forwarded-For"); got != "198.51.100.45" {
		t.Fatalf("x-forwarded-for = %q", got)
	}
	if got := request.Header.Get("X-Forwarded-Proto"); got != "http" {
		t.Fatalf("x-forwarded-proto = %q", got)
	}
	if got := request.Header.Get("X-Forwarded-Host"); got != "web.codespace.gateway.example.test" {
		t.Fatalf("x-forwarded-host = %q", got)
	}
}

func TestGatewayProxyRequestRebuildsCookieHeader(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "https://codespace.gateway.example.test/", nil)
	request.RemoteAddr = "198.51.100.44:3000"
	request.Header.Add("Cookie", "app=ok; "+gatewaySessionCookieName+"=plain-session; "+gatewaySecureSessionCookieName+"=secure-session")
	request.Header.Add("Cookie", gatewayReturnToCookieName+"=%2Fdeep; "+gatewaySecureReturnToCookieName+"=%2Fsecure; theme=dark; broken")

	prepareGatewayProxyRequest(request, gatewayProxyRequestContext{
		codespaceUUID:  "codespace-uuid",
		endpointID:     "web",
		access:         "authenticated",
		userID:         42,
		externalScheme: "https",
	})
	if got := request.Header.Values("Cookie"); len(got) != 1 || got[0] != "app=ok; theme=dark" {
		t.Fatalf("cookie header = %#v", got)
	}
}

func TestGatewayProxyRequestDeletesCookieHeaderWhenOnlyReservedOrInvalid(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "https://codespace.gateway.example.test/", nil)
	request.RemoteAddr = "198.51.100.44:3000"
	request.Header.Add("Cookie", gatewaySessionCookieName+"=plain-session; broken")
	request.Header.Add("Cookie", gatewaySecureReturnToCookieName+"=%2F")

	prepareGatewayProxyRequest(request, gatewayProxyRequestContext{
		codespaceUUID:  "codespace-uuid",
		endpointID:     "web",
		access:         "public",
		externalScheme: "https",
	})
	if got := request.Header.Values("Cookie"); len(got) != 0 {
		t.Fatalf("cookie header = %#v", got)
	}
}

func TestGatewayProxyResponseRewritesInternalLocation(t *testing.T) {
	t.Parallel()

	proxyContext := gatewayProxyResponseContext{
		externalScheme: "https",
		externalHost:   "11111111-1111-4111-8111-111111111111.gateway.example.test",
		upstreamScheme: "http",
		upstreamHost:   "runtime.internal:3000",
	}
	tests := []struct {
		name     string
		location string
		want     string
	}{
		{
			name:     "relative path",
			location: "/login?next=/",
			want:     "/login?next=/",
		},
		{
			name:     "relative segment",
			location: "../next",
			want:     "../next",
		},
		{
			name:     "internal absolute",
			location: "http://runtime.internal:3000/login?next=%2F#top",
			want:     "https://11111111-1111-4111-8111-111111111111.gateway.example.test/login?next=%2F#top",
		},
		{
			name:     "internal network path",
			location: "//runtime.internal:3000/login",
			want:     "https://11111111-1111-4111-8111-111111111111.gateway.example.test/login",
		},
		{
			name:     "external absolute",
			location: "https://idp.example.test/login",
			want:     "https://idp.example.test/login",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{"Location": {test.location}}
			normalizeGatewayProxyResponse(header, proxyContext)
			if got := header.Get("Location"); got != test.want {
				t.Fatalf("location = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGatewayProxyResponseRewritesDefaultPortLocation(t *testing.T) {
	t.Parallel()

	header := http.Header{"Location": {"http://runtime.internal/login"}}
	normalizeGatewayProxyResponse(header, gatewayProxyResponseContext{
		externalScheme: "https",
		externalHost:   "codespace.gateway.example.test:8443",
		upstreamScheme: "http",
		upstreamHost:   "runtime.internal:80",
	})
	if got := header.Get("Location"); got != "https://codespace.gateway.example.test:8443/login" {
		t.Fatalf("location = %q", got)
	}
}

func TestGatewayProxyResponseDeletesServiceWorkerAllowedAndKeepsRuntimeCORS(t *testing.T) {
	t.Parallel()

	header := http.Header{
		"Access-Control-Allow-Origin":  {"https://app.example.test"},
		"Access-Control-Allow-Headers": {"X-App-Header"},
		"Service-Worker-Allowed":       {"/"},
		"Vary":                         {"Origin"},
	}
	normalizeGatewayProxyResponse(header, gatewayProxyResponseContext{})
	if got := header.Get("Service-Worker-Allowed"); got != "" {
		t.Fatalf("service worker allowed = %q", got)
	}
	if got := header.Get("Access-Control-Allow-Origin"); got != "https://app.example.test" {
		t.Fatalf("access control allow origin = %q", got)
	}
	if got := header.Get("Access-Control-Allow-Headers"); got != "X-App-Header" {
		t.Fatalf("access control allow headers = %q", got)
	}
	if got := header.Get("Vary"); got != "Origin" {
		t.Fatalf("vary = %q", got)
	}
}

func TestGatewayProxyResponseRebuildsSetCookieForHTTPS(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Add("Set-Cookie", gatewaySessionCookieName+"=reserved")
	header.Add("Set-Cookie", "app=ok; Domain=example.test; Path=app; SameSite=Bogus")
	header.Add("Set-Cookie", "theme=dark; Domain=.example.test; Path=/ui; Max-Age=60; HttpOnly; SameSite=Strict")
	header.Add("Set-Cookie", "__Host-app=ok; Domain=example.test; Path=/wrong")
	header.Add("Set-Cookie", "__Secure-app=ok")
	header.Add("Set-Cookie", "part=ok; Partitioned")

	normalizeGatewayProxyResponse(header, gatewayProxyResponseContext{externalScheme: "https"})

	cookies := parseSetCookiesByName(t, header.Values("Set-Cookie"))
	if _, ok := cookies[gatewaySessionCookieName]; ok {
		t.Fatalf("reserved session cookie was returned")
	}
	app := cookies["app"]
	if app == nil {
		t.Fatalf("app cookie missing")
	}
	if app.Domain != "" || app.Path != "/" || !app.Secure || app.SameSite != http.SameSiteLaxMode {
		t.Fatalf("app cookie = %#v", app)
	}
	theme := cookies["theme"]
	if theme == nil {
		t.Fatalf("theme cookie missing")
	}
	if theme.Domain != "" || theme.Path != "/ui" || theme.MaxAge != 60 || !theme.HttpOnly ||
		!theme.Secure || theme.SameSite != http.SameSiteStrictMode {
		t.Fatalf("theme cookie = %#v", theme)
	}
	hostCookie := cookies["__Host-app"]
	if hostCookie == nil {
		t.Fatalf("__Host-app cookie missing")
	}
	if hostCookie.Domain != "" || hostCookie.Path != "/" || !hostCookie.Secure {
		t.Fatalf("__Host-app cookie = %#v", hostCookie)
	}
	secureCookie := cookies["__Secure-app"]
	if secureCookie == nil || !secureCookie.Secure {
		t.Fatalf("__Secure-app cookie = %#v", secureCookie)
	}
	partitionedCookie := cookies["part"]
	if partitionedCookie == nil || !partitionedCookie.Secure || !partitionedCookie.Partitioned {
		t.Fatalf("partitioned cookie = %#v", partitionedCookie)
	}
}

func TestGatewayProxyResponseRebuildsSetCookieForHTTP(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Add("Set-Cookie", "plain=ok; Domain=example.test; Path=/app")
	header.Add("Set-Cookie", "secure=keep; Secure; Path=/")
	header.Add("Set-Cookie", "none=bad; SameSite=None")
	header.Add("Set-Cookie", "__Host-app=bad")
	header.Add("Set-Cookie", "__Secure-app=bad")
	header.Add("Set-Cookie", "part=bad; Partitioned")
	header.Add("Set-Cookie", "bad name=value")

	normalizeGatewayProxyResponse(header, gatewayProxyResponseContext{externalScheme: "http"})

	cookies := parseSetCookiesByName(t, header.Values("Set-Cookie"))
	if len(cookies) != 2 {
		t.Fatalf("cookies = %#v", header.Values("Set-Cookie"))
	}
	plain := cookies["plain"]
	if plain == nil || plain.Domain != "" || plain.Path != "/app" || plain.Secure {
		t.Fatalf("plain cookie = %#v", plain)
	}
	secureCookie := cookies["secure"]
	if secureCookie == nil || !secureCookie.Secure || secureCookie.Path != "/" {
		t.Fatalf("secure cookie = %#v", secureCookie)
	}
	for _, name := range []string{"none", "__Host-app", "__Secure-app", "part", "bad name"} {
		if cookies[name] != nil {
			t.Fatalf("cookie %q was returned: %#v", name, cookies[name])
		}
	}
}

func parseSetCookiesByName(t *testing.T, values []string) map[string]*http.Cookie {
	t.Helper()

	cookies := make(map[string]*http.Cookie, len(values))
	for _, value := range values {
		cookie, err := http.ParseSetCookie(value)
		if err != nil {
			t.Fatalf("parse set-cookie %q: %v", value, err)
		}
		cookies[cookie.Name] = cookie
	}
	return cookies
}
