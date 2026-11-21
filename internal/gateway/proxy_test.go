// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayProxyUpstreamCancellationReturnsBadGateway(t *testing.T) {
	for _, routeChanged := range []bool{false, true} {
		name := "agent canceled"
		if routeChanged {
			name = "route changed"
		}
		t.Run(name, func(t *testing.T) {
			routes := newRouteStore(gatewayTestBackend{endpointError: context.Canceled})
			defer routes.Close()
			snapshot := testGatewayRuntime()
			snapshot.Endpoints = []endpointSnapshot{{EndpointID: "app", Public: true}}
			require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))
			route, ok := routes.Get(snapshot.RuntimeKey, "app")
			require.True(t, ok)
			request := httptest.NewRequest(http.MethodGet, "http://workspace.test/", nil)
			if routeChanged {
				_, changed, release, ok := routes.BeginProxy(request, route.RuntimeKey, route.EndpointID)
				require.True(t, ok)
				defer release()
				require.NoError(t, routes.ReplaceSharedGatewayRuntimes(nil))
				<-changed.Context().Done()
				request = changed
				require.NoError(t, routes.ReplaceSharedGatewayRuntimes([]runtimeSnapshot{snapshot}))
			}
			response := httptest.NewRecorder()
			proxyGatewayEndpoint(response, request, routes, route, gatewayProxyRequestContext{})
			require.Equal(t, http.StatusBadGateway, response.Code)
			require.JSONEq(t, `{"error":"gateway upstream unavailable"}`, response.Body.String())
		})
	}
}

func TestGatewayProxyRequestRebuildsContext(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name, scheme, host, externalHost, port string
		authenticated                          bool
	}{
		{name: "HTTP", scheme: "http", host: "workspace.example.test", port: "80"},
		{name: "HTTPS authenticated", scheme: "https", host: "workspace.example.test", port: "443", authenticated: true},
		{name: "HTTP explicit port", scheme: "http", host: "workspace.example.test:8081", port: "8081"},
		{name: "HTTPS explicit port", scheme: "https", host: "workspace.example.test:8443", port: "8443"},
		{name: "external host override", scheme: "https", host: "internal.example.test:8080", externalHost: "workspace.example.test:9443", port: "9443"},
		{name: "IPv6", scheme: "https", host: "[2001:db8::1]", port: "443"},
		{name: "IPv6 explicit port", scheme: "https", host: "[2001:db8::1]:8443", port: "8443"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/work", nil)
			request.RemoteAddr = "198.51.100.44:3000"
			for _, header := range []string{proxyHeaderCodespaceID, proxyHeaderEndpointID, proxyHeaderAccess, proxyHeaderUserID, "Forwarded", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host"} {
				request.Header.Set(header, "spoofed")
			}
			request.Header.Add("X-Forwarded-Port", "8080")
			request.Header.Add("X-Forwarded-Port", "1234")
			proxyContext := gatewayProxyRequestContext{
				runtimeKey: "site/123", endpointID: "workspace", access: "public",
				externalScheme: test.scheme, externalHost: test.externalHost,
			}
			userID := ""
			if test.authenticated {
				proxyContext.access, proxyContext.userID, userID = "authenticated", 42, "42"
			}

			prepareGatewayProxyRequest(request, proxyContext)

			host := test.host
			if test.externalHost != "" {
				host = test.externalHost
			}
			for header, expected := range map[string]string{
				proxyHeaderCodespaceID: "123", proxyHeaderEndpointID: "workspace",
				proxyHeaderAccess: proxyContext.access, proxyHeaderUserID: userID,
				"Forwarded": "", "X-Forwarded-For": "198.51.100.44",
				"X-Forwarded-Proto": test.scheme, "X-Forwarded-Host": host,
			} {
				require.Equal(t, expected, request.Header.Get(header), header)
			}
			require.Equal(t, []string{test.port}, request.Header.Values("X-Forwarded-Port"))
		})
	}
}

func TestGatewayProxyRequestRebuildsCookieHeader(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "https://codespace.gateway.example.test/", nil)
	request.RemoteAddr = "198.51.100.44:3000"
	request.Header.Add("Cookie", "app=ok; "+sessionCookieName+"=plain-session; "+secureSessionCookieName+"=secure-session")
	request.Header.Add("Cookie", returnToCookieName+"=%2Fdeep; "+secureReturnToCookieName+"=%2Fsecure; theme=dark; broken")

	prepareGatewayProxyRequest(request, gatewayProxyRequestContext{
		runtimeKey:     "site-1/1",
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
	request.Header.Add("Cookie", sessionCookieName+"=plain-session; broken")
	request.Header.Add("Cookie", secureReturnToCookieName+"=%2F")

	prepareGatewayProxyRequest(request, gatewayProxyRequestContext{
		runtimeKey:     "site-1/1",
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
		upstreamHost:   "runtime.internal:80",
	})
	if got := header.Get("Location"); got != "https://codespace.gateway.example.test:8443/login" {
		t.Fatalf("location = %q", got)
	}
}

func TestGatewayProxyResponseKeepsProviderHeaders(t *testing.T) {
	t.Parallel()

	header := http.Header{
		"Access-Control-Allow-Origin":  {"https://app.example.test"},
		"Access-Control-Allow-Headers": {"X-App-Header"},
		"Service-Worker-Allowed":       {"/"},
		"Vary":                         {"Origin"},
	}
	normalizeGatewayProxyResponse(header, gatewayProxyResponseContext{})
	if got := header.Get("Service-Worker-Allowed"); got != "/" {
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
	header.Add("Set-Cookie", sessionCookieName+"=reserved")
	header.Add("Set-Cookie", "app=ok; Domain=example.test; Path=app; SameSite=Bogus")
	header.Add("Set-Cookie", "theme=dark; Domain=.example.test; Path=/ui; Max-Age=60; HttpOnly; SameSite=Strict")
	header.Add("Set-Cookie", "__Host-app=ok; Domain=example.test; Path=/wrong")
	header.Add("Set-Cookie", "__Secure-app=ok")
	header.Add("Set-Cookie", "part=ok; Partitioned")

	normalizeGatewayProxyResponse(header, gatewayProxyResponseContext{externalScheme: "https"})

	cookies := parseSetCookiesByName(t, header.Values("Set-Cookie"))
	if _, ok := cookies[sessionCookieName]; ok {
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
