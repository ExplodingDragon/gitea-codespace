// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
)

func TestGatewayOpenCreatesSessionAndWorkspaceRevalidates(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:                42,
					CodespaceUuid:         codespaceUUID,
					EndpointId:            "workspace",
					InteractionGeneration: 7,
				},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	sessions := newGatewaySessionRegistry()
	handler := newGatewayHandler(newProcessHealth(), sessions, newTestGatewayAccess(), controlPlane)

	openRequest := httptest.NewRequest(http.MethodGet, "/open?code=open-code", nil)
	openResponse := httptest.NewRecorder()
	handler.ServeHTTP(openResponse, openRequest)
	if openResponse.Code != http.StatusSeeOther {
		t.Fatalf("open status = %d", openResponse.Code)
	}
	assertGatewayOpenHeaders(t, openResponse)
	if location := openResponse.Header().Get("Location"); location != "/w/"+codespaceUUID+"/" {
		t.Fatalf("open redirect = %q", location)
	}
	cookies := openResponse.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != gatewaySessionCookieName || cookies[0].Value == "" {
		t.Fatalf("session cookies = %#v", cookies)
	}

	workspaceRequest := httptest.NewRequest(http.MethodGet, "/w/"+codespaceUUID+"/", nil)
	workspaceRequest.AddCookie(cookies[0])
	workspaceResponse := httptest.NewRecorder()
	handler.ServeHTTP(workspaceResponse, workspaceRequest)
	if workspaceResponse.Code != http.StatusOK {
		t.Fatalf("workspace status = %d body=%s", workspaceResponse.Code, workspaceResponse.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(workspaceResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode workspace response: %v", err)
	}
	if payload["codespace_uuid"] != codespaceUUID || payload["endpoint_id"] != "workspace" || payload["status"] != "authorized" {
		t.Fatalf("workspace payload = %#v", payload)
	}
	if service.revalidateRequest.GetEndpoint().GetUserId() != 42 ||
		service.revalidateRequest.GetEndpoint().GetCodespaceUuid() != codespaceUUID ||
		service.revalidateRequest.GetEndpoint().GetEndpointId() != "workspace" {
		t.Fatalf("revalidate request = %#v", service.revalidateRequest)
	}
	if live := sessions.LiveSessions(codespaceUUID); live != 0 {
		t.Fatalf("live sessions after request = %d", live)
	}

	secondWorkspaceResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondWorkspaceResponse, workspaceRequest.Clone(workspaceRequest.Context()))
	if secondWorkspaceResponse.Code != http.StatusOK {
		t.Fatalf("cached workspace status = %d", secondWorkspaceResponse.Code)
	}
	if calls := service.revalidateCallCount(); calls != 1 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}

}

func TestGatewayOpenDeniedDoesNotCreateSession(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Denied{
				Denied: &codespacev1.FailureDetail{Category: "state_unavailable"},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane)

	request := httptest.NewRequest(http.MethodGet, "/open?code=open-code", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("open denied status = %d", response.Code)
	}
	assertGatewayOpenHeaders(t, response)
	if cookies := response.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("open denied cookies = %#v", cookies)
	}
}

func TestGatewayOpenRejectsInvalidCodeRequestWithoutRPC(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: "11111111-1111-4111-8111-111111111111",
					EndpointId:    "workspace",
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane)

	tests := []struct {
		name   string
		method string
		target string
		status int
	}{
		{name: "missing", method: http.MethodGet, target: "/open", status: http.StatusForbidden},
		{name: "empty", method: http.MethodGet, target: "/open?code=", status: http.StatusForbidden},
		{name: "space", method: http.MethodGet, target: "/open?code=+", status: http.StatusForbidden},
		{name: "duplicate", method: http.MethodGet, target: "/open?code=one&code=two", status: http.StatusForbidden},
		{name: "extra", method: http.MethodGet, target: "/open?code=one&next=/", status: http.StatusForbidden},
		{name: "method", method: http.MethodPost, target: "/open?code=one", status: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.target, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d", response.Code)
			}
			assertGatewayOpenHeaders(t, response)
		})
	}
	if calls := service.openTokenCallCount(); calls != 0 {
		t.Fatalf("open token rpc calls = %d", calls)
	}
}

func TestGatewayOpenRejectsServiceWorkerWithoutRPC(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: "11111111-1111-4111-8111-111111111111",
					EndpointId:    "workspace",
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane)

	request := httptest.NewRequest(http.MethodGet, "/open?code=open-code", nil)
	request.Header.Set("Service-Worker", "script")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("open service worker status = %d", response.Code)
	}
	assertGatewayOpenHeaders(t, response)
	if calls := service.openTokenCallCount(); calls != 0 {
		t.Fatalf("open token rpc calls = %d", calls)
	}
}

func TestGatewayOpenRequiresValidHostWhenConfigured(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://other.example.test/.gitea-codespace/open?code=open-code", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("open invalid host status = %d", response.Code)
	}
	if calls := service.openTokenCallCount(); calls != 0 {
		t.Fatalf("open token rpc calls = %d", calls)
	}
}

func TestGatewayOpenRejectsHostBindingMismatch(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	response := httptest.NewRecorder()
	target := "http://web-" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/.gitea-codespace/open?code=open-code"
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("open host mismatch status = %d", response.Code)
	}
	if calls := service.openTokenCallCount(); calls != 1 {
		t.Fatalf("open token rpc calls = %d", calls)
	}
}

func TestGatewayOpenWithConfiguredHostUsesReservedPath(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	legacyTarget := "http://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/open?code=open-code"
	legacyResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyResponse, httptest.NewRequest(http.MethodGet, legacyTarget, nil))
	if legacyResponse.Code != http.StatusNotFound {
		t.Fatalf("legacy open status = %d", legacyResponse.Code)
	}
	if calls := service.openTokenCallCount(); calls != 0 {
		t.Fatalf("legacy open token rpc calls = %d", calls)
	}

	target := "http://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/.gitea-codespace/open?code=open-code"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("reserved open status = %d body=%s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/" {
		t.Fatalf("reserved open redirect = %q", location)
	}
	if calls := service.openTokenCallCount(); calls != 1 {
		t.Fatalf("reserved open token rpc calls = %d", calls)
	}
}

func TestGatewayWorkspaceMissingSessionRedirectsBrowserNavigationToGitea(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	browserAuth := newGatewayBrowserAuth()
	if err := browserAuth.SaveManagerServiceSettings(manager.ManagerServiceSettings{GiteaWebURL: "https://gitea.example.test/git/"}); err != nil {
		t.Fatalf("save browser auth settings: %v", err)
	}
	handler := newGatewayHandlerWithOriginAndBrowserAuth(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy, browserAuth)

	target := "http://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/w/?folder=%2Fworkspace"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	setGatewayBrowserNavigationHeaders(request.Header)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("workspace missing session status = %d body=%s", response.Code, response.Body.String())
	}
	wantLocation := "https://gitea.example.test/git/-/codespaces/" + codespaceUUID + "/open"
	if location := response.Header().Get("Location"); location != wantLocation {
		t.Fatalf("auth recovery location = %q", location)
	}
	cookies := parseSetCookiesByName(t, response.Header().Values("Set-Cookie"))
	returnTo := cookies[gatewayReturnToCookieName]
	if returnTo == nil || returnTo.Value != "/w/?folder=%2Fworkspace" || returnTo.Domain != "" || returnTo.Path != "/" || returnTo.Secure {
		t.Fatalf("return-to cookie = %#v", returnTo)
	}
	if calls := service.revalidateCallCount(); calls != 0 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}
}

func TestGatewayWorkspaceMissingSessionUsesSecureReturnToCookieForHTTPS(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "https://gateway.example.test")
	browserAuth := newGatewayBrowserAuth()
	if err := browserAuth.SaveManagerServiceSettings(manager.ManagerServiceSettings{GiteaWebURL: "https://gitea.example.test/git/"}); err != nil {
		t.Fatalf("save browser auth settings: %v", err)
	}
	handler := newGatewayHandlerWithOriginAndBrowserAuth(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy, browserAuth)

	target := "https://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/w/?folder=%2Fworkspace"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	setGatewayBrowserNavigationHeaders(request.Header)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("workspace missing session status = %d body=%s", response.Code, response.Body.String())
	}
	wantLocation := "https://gitea.example.test/git/-/codespaces/" + codespaceUUID + "/open"
	if location := response.Header().Get("Location"); location != wantLocation {
		t.Fatalf("auth recovery location = %q", location)
	}
	cookies := parseSetCookiesByName(t, response.Header().Values("Set-Cookie"))
	if _, ok := cookies[gatewayReturnToCookieName]; ok {
		t.Fatalf("plain return-to cookie was set for https")
	}
	returnTo := cookies[gatewaySecureReturnToCookieName]
	if returnTo == nil ||
		returnTo.Value != "/w/?folder=%2Fworkspace" ||
		returnTo.Domain != "" ||
		returnTo.Path != "/" ||
		!returnTo.Secure ||
		!returnTo.HttpOnly {
		t.Fatalf("secure return-to cookie = %#v", returnTo)
	}
}

func TestGatewayWorkspaceMissingSessionKeepsNonNavigationUnauthorized(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	browserAuth := newGatewayBrowserAuth()
	if err := browserAuth.SaveManagerServiceSettings(manager.ManagerServiceSettings{GiteaWebURL: "https://gitea.example.test/"}); err != nil {
		t.Fatalf("save browser auth settings: %v", err)
	}
	handler := newGatewayHandlerWithOriginAndBrowserAuth(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy, browserAuth)

	target := "http://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/w/"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("non-navigation missing session status = %d body=%s", response.Code, response.Body.String())
	}
	if cookies := response.Header().Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("non-navigation set-cookie = %#v", cookies)
	}
}

func TestGatewayOpenRedirectsToReturnToCookie(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	target := "http://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/.gitea-codespace/open?code=open-code"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.AddCookie(&http.Cookie{Name: gatewayReturnToCookieName, Value: "/w/?folder=%2Fworkspace"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("open status = %d body=%s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/w/?folder=%2Fworkspace" {
		t.Fatalf("open redirect = %q", location)
	}
	cookies := parseSetCookiesByName(t, response.Header().Values("Set-Cookie"))
	if session := cookies[gatewaySessionCookieName]; session == nil || session.Value == "" {
		t.Fatalf("session cookie = %#v", session)
	}
	if clear := cookies[gatewayReturnToCookieName]; clear == nil || clear.MaxAge != -1 {
		t.Fatalf("return-to clear cookie = %#v", clear)
	}
}

func TestGatewayOpenClearsOtherSchemeReturnToCookieWithoutReadingIt(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	target := "http://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/.gitea-codespace/open?code=open-code"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.AddCookie(&http.Cookie{Name: gatewaySecureReturnToCookieName, Value: "/w/secret"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("open status = %d body=%s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/" {
		t.Fatalf("open redirect = %q", location)
	}
	cookies := parseSetCookiesByName(t, response.Header().Values("Set-Cookie"))
	if session := cookies[gatewaySessionCookieName]; session == nil || session.Value == "" {
		t.Fatalf("session cookie = %#v", session)
	}
	if clear := cookies[gatewayReturnToCookieName]; clear == nil || clear.MaxAge != -1 {
		t.Fatalf("return-to clear cookie = %#v", clear)
	}
	if clear := cookies[gatewaySecureReturnToCookieName]; clear == nil || clear.MaxAge != -1 || !clear.Secure {
		t.Fatalf("secure return-to clear cookie = %#v", clear)
	}
}

func TestGatewayOpenDuplicateReturnToCookieFallsBackToRoot(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	target := "http://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/.gitea-codespace/open?code=open-code"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Add("Cookie", gatewayReturnToCookieName+"=/w/one; "+gatewayReturnToCookieName+"=/w/two")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("open status = %d body=%s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/" {
		t.Fatalf("open redirect = %q", location)
	}
	cookies := parseSetCookiesByName(t, response.Header().Values("Set-Cookie"))
	if session := cookies[gatewaySessionCookieName]; session == nil || session.Value == "" {
		t.Fatalf("session cookie = %#v", session)
	}
	if clear := cookies[gatewayReturnToCookieName]; clear == nil || clear.MaxAge != -1 {
		t.Fatalf("return-to clear cookie = %#v", clear)
	}
}

func TestGatewayOpenInvalidCodeClearsReturnToCookieWithoutRPC(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	target := "http://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/.gitea-codespace/open?code=open-code&extra=1"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.AddCookie(&http.Cookie{Name: gatewayReturnToCookieName, Value: "/w/"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("open status = %d body=%s", response.Code, response.Body.String())
	}
	cookies := parseSetCookiesByName(t, response.Header().Values("Set-Cookie"))
	if clear := cookies[gatewayReturnToCookieName]; clear == nil || clear.MaxAge != -1 {
		t.Fatalf("return-to clear cookie = %#v", clear)
	}
	if _, ok := cookies[gatewaySessionCookieName]; ok {
		t.Fatalf("session cookie was set on invalid code")
	}
	if calls := service.openTokenCallCount(); calls != 0 {
		t.Fatalf("open token rpc calls = %d", calls)
	}
}

func TestGatewayOpenUsesSecureSessionCookieForHTTPSGateway(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "https://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	cookie := openGatewaySessionWithOrigin(t, handler, codespaceUUID, policy)
	if cookie.Name != gatewaySecureSessionCookieName || !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" {
		t.Fatalf("secure session cookie = %#v", cookie)
	}

	target := "https://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/w/"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", "https://"+gatewayTestUUID32(codespaceUUID)+".gateway.example.test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("workspace status = %d body=%s", response.Code, response.Body.String())
	}
	if calls := service.revalidateCallCount(); calls != 1 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}

	plainCookieRequest := httptest.NewRequest(http.MethodGet, target, nil)
	plainCookieRequest.AddCookie(&http.Cookie{Name: gatewaySessionCookieName, Value: cookie.Value})
	plainCookieRequest.Header.Set("Origin", "https://"+gatewayTestUUID32(codespaceUUID)+".gateway.example.test")
	plainCookieResponse := httptest.NewRecorder()
	handler.ServeHTTP(plainCookieResponse, plainCookieRequest)
	if plainCookieResponse.Code != http.StatusUnauthorized {
		t.Fatalf("plain cookie on https status = %d body=%s", plainCookieResponse.Code, plainCookieResponse.Body.String())
	}
	if calls := service.revalidateCallCount(); calls != 1 {
		t.Fatalf("revalidate rpc calls after plain cookie = %d", calls)
	}
}

func TestGatewayOpenUsesGlobalInflightLimit(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	service := &gatewayManagerService{
		openTokenStarted: started,
		openTokenRelease: release,
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: "11111111-1111-4111-8111-111111111111",
					EndpointId:    "workspace",
				},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(
		newProcessHealth(),
		newGatewaySessionRegistry(),
		newGatewayAccessController(gatewayAccessConfig{
			allowedTTL:                      time.Second,
			maxInflightTotal:                1,
			maxInflightPerSession:           1,
			publicMaxConnectionsPerEndpoint: 1,
			publicMaxConnectionsPerIP:       1,
			validationMaxInflight:           1,
		}),
		controlPlane,
	)

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- serveGatewayOpenRequest(handler)
	}()
	<-started

	if status := serveGatewayOpenRequest(handler); status != http.StatusServiceUnavailable {
		t.Fatalf("open status while capacity full = %d", status)
	}
	release <- struct{}{}
	if status := <-firstDone; status != http.StatusSeeOther {
		t.Fatalf("first open status = %d", status)
	}
	if calls := service.openTokenCallCount(); calls != 1 {
		t.Fatalf("open token rpc calls = %d", calls)
	}
}

func TestGatewayPublicEndpointValidatesWithoutLiveSession(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{
				Allowed: &codespacev1.PublicEndpointAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	sessions := newGatewaySessionRegistry()
	handler := newGatewayHandler(newProcessHealth(), sessions, newTestGatewayAccess(), controlPlane)

	request := httptest.NewRequest(http.MethodGet, "/p/"+codespaceUUID+"/web/", nil)
	request.AddCookie(&http.Cookie{Name: gatewaySessionCookieName, Value: "old-session"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("public endpoint status = %d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode public endpoint response: %v", err)
	}
	if payload["access"] != "public" ||
		payload["codespace_uuid"] != codespaceUUID ||
		payload["endpoint_id"] != "web" ||
		payload["status"] != "authorized" {
		t.Fatalf("public endpoint payload = %#v", payload)
	}
	if service.publicEndpointRequest.GetProtocolVersion() != 1 ||
		service.publicEndpointRequest.GetCodespaceUuid() != codespaceUUID ||
		service.publicEndpointRequest.GetEndpointId() != "web" {
		t.Fatalf("public endpoint request = %#v", service.publicEndpointRequest)
	}
	if live := sessions.LiveSessions(codespaceUUID); live != 0 {
		t.Fatalf("public endpoint live sessions = %d", live)
	}
	clearCookie := response.Result().Cookies()[0]
	if clearCookie.Name != gatewaySessionCookieName || clearCookie.MaxAge != -1 {
		t.Fatalf("public endpoint clear cookie = %#v", clearCookie)
	}

	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, httptest.NewRequest(http.MethodGet, "/p/"+codespaceUUID+"/web/", nil))
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("cached public endpoint status = %d", secondResponse.Code)
	}
	if calls := service.publicEndpointCallCount(); calls != 1 {
		t.Fatalf("public endpoint rpc calls = %d", calls)
	}
}

func TestGatewayPublicEndpointDeniedReturnsNotFound(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Denied{
				Denied: &codespacev1.FailureDetail{Category: "state_unavailable"},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/p/codespace-uuid/web/", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("public endpoint denied status = %d", response.Code)
	}
}

func TestGatewayPublicEndpointRejectsWorkspaceWithoutRPC(t *testing.T) {
	t.Parallel()

	service := &gatewayManagerService{
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{
				Allowed: &codespacev1.PublicEndpointAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/p/codespace-uuid/workspace/", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("workspace public endpoint status = %d", response.Code)
	}
	if calls := service.publicEndpointCallCount(); calls != 0 {
		t.Fatalf("public endpoint rpc calls = %d", calls)
	}
}

func TestGatewayPublicEndpointRejectsHostBindingMismatchWithoutRPC(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{
				Allowed: &codespacev1.PublicEndpointAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	target := "http://other-" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/p/" + codespaceUUID + "/web/"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("public endpoint host mismatch status = %d", response.Code)
	}
	if calls := service.publicEndpointCallCount(); calls != 0 {
		t.Fatalf("public endpoint rpc calls = %d", calls)
	}
}

func TestGatewayPublicEndpointUsesHostBindingWithRootCompatPath(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{
				Allowed: &codespacev1.PublicEndpointAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler := newGatewayHandlerWithOrigin(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane, policy)

	target := "http://web-" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/p/"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("public endpoint status = %d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode public endpoint response: %v", err)
	}
	if payload["codespace_uuid"] != codespaceUUID || payload["endpoint_id"] != "web" || payload["access"] != "public" {
		t.Fatalf("public endpoint payload = %#v", payload)
	}
	if calls := service.publicEndpointCallCount(); calls != 1 {
		t.Fatalf("public endpoint rpc calls = %d", calls)
	}
}

func TestGatewayPublicEndpointServiceWorkerHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    func(http.Header)
		status    int
		wantCalls int
	}{
		{
			name: "service worker script",
			header: func(header http.Header) {
				header.Set("Service-Worker", "script")
			},
			status: http.StatusForbidden,
		},
		{
			name: "service worker malformed value",
			header: func(header http.Header) {
				header.Set("Service-Worker", "bogus")
			},
			status: http.StatusForbidden,
		},
		{
			name: "fetch dest serviceworker",
			header: func(header http.Header) {
				header.Set("Sec-Fetch-Dest", "serviceworker")
			},
			status: http.StatusForbidden,
		},
		{
			name: "fetch dest duplicate",
			header: func(header http.Header) {
				header.Add("Sec-Fetch-Dest", "worker")
				header.Add("Sec-Fetch-Dest", "serviceworker")
			},
			status: http.StatusForbidden,
		},
		{
			name: "ordinary worker",
			header: func(header http.Header) {
				header.Set("Sec-Fetch-Dest", "worker")
			},
			status:    http.StatusOK,
			wantCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &gatewayManagerService{
				publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
					Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{
						Allowed: &codespacev1.PublicEndpointAllowed{},
					},
				},
			}
			controlPlane, closeServer := newTestGatewayControlPlane(t, service)
			defer closeServer()
			handler := newGatewayHandler(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane)

			request := httptest.NewRequest(http.MethodGet, "/p/codespace-uuid/web/", nil)
			request.RemoteAddr = "198.51.100.10:1000"
			test.header(request.Header)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("public endpoint status = %d", response.Code)
			}
			if calls := service.publicEndpointCallCount(); calls != test.wantCalls {
				t.Fatalf("public endpoint rpc calls = %d", calls)
			}
		})
	}
}

func TestGatewayWorkspaceRejectsServiceWorkerWithoutRevalidate(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	sessions := newGatewaySessionRegistry()
	handler := newGatewayHandler(newProcessHealth(), sessions, newTestGatewayAccess(), controlPlane)
	cookie := openGatewaySession(t, handler)

	request := httptest.NewRequest(http.MethodGet, "/w/"+codespaceUUID+"/", nil)
	request.AddCookie(cookie)
	request.Header.Set("Sec-Fetch-Dest", "serviceworker")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("workspace service worker status = %d", response.Code)
	}
	if calls := service.revalidateCallCount(); calls != 0 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}
	if live := sessions.LiveSessions(codespaceUUID); live != 0 {
		t.Fatalf("live sessions after service worker request = %d", live)
	}
}

func TestGatewayWorkspaceSourcePolicy(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	tests := []struct {
		name       string
		method     string
		target     string
		header     func(http.Header)
		status     int
		revalidate int
	}{
		{
			name:   "exact origin get",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Origin", "http://workspace.gateway.test")
			},
			status:     http.StatusOK,
			revalidate: 1,
		},
		{
			name:   "exact default port origin",
			method: http.MethodGet,
			target: "https://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Origin", "https://workspace.gateway.test:443")
			},
			status:     http.StatusOK,
			revalidate: 1,
		},
		{
			name:   "cross origin get",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Origin", "http://other.gateway.test")
			},
			status: http.StatusForbidden,
		},
		{
			name:   "duplicate origin",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Add("Origin", "http://workspace.gateway.test")
				header.Add("Origin", "http://workspace.gateway.test")
			},
			status: http.StatusForbidden,
		},
		{
			name:   "null origin",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Origin", "null")
			},
			status: http.StatusForbidden,
		},
		{
			name:   "post without origin",
			method: http.MethodPost,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(http.Header) {
			},
			status: http.StatusForbidden,
		},
		{
			name:   "post with exact origin",
			method: http.MethodPost,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Origin", "http://workspace.gateway.test")
			},
			status:     http.StatusOK,
			revalidate: 1,
		},
		{
			name:   "same origin fetch metadata",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Sec-Fetch-Site", "same-origin")
			},
			status:     http.StatusOK,
			revalidate: 1,
		},
		{
			name:   "top navigation fetch metadata",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Sec-Fetch-Site", "cross-site")
				header.Set("Sec-Fetch-Mode", "navigate")
				header.Set("Sec-Fetch-Dest", "document")
			},
			status:     http.StatusOK,
			revalidate: 1,
		},
		{
			name:   "no browser source metadata",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(http.Header) {
			},
			status:     http.StatusOK,
			revalidate: 1,
		},
		{
			name:   "conflicting fetch metadata",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Sec-Fetch-Site", "cross-site")
				header.Set("Sec-Fetch-Mode", "cors")
				header.Set("Sec-Fetch-Dest", "empty")
			},
			status: http.StatusForbidden,
		},
		{
			name:   "websocket without origin",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Connection", "Upgrade")
				header.Set("Upgrade", "websocket")
			},
			status: http.StatusForbidden,
		},
		{
			name:   "websocket with exact origin",
			method: http.MethodGet,
			target: "http://workspace.gateway.test/w/" + codespaceUUID + "/",
			header: func(header http.Header) {
				header.Set("Connection", "Upgrade")
				header.Set("Upgrade", "websocket")
				header.Set("Origin", "http://workspace.gateway.test")
			},
			status:     http.StatusOK,
			revalidate: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, service, cookie := newGatewayWorkspaceSourceTestHandler(t, codespaceUUID)

			request := httptest.NewRequest(test.method, test.target, nil)
			request.AddCookie(cookie)
			test.header(request.Header)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("workspace status = %d body=%s", response.Code, response.Body.String())
			}
			if calls := service.revalidateCallCount(); calls != test.revalidate {
				t.Fatalf("revalidate rpc calls = %d", calls)
			}
		})
	}
}

func TestGatewayWorkspaceSourcePolicyUsesGatewayURL(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	policy := mustGatewayOriginPolicy(t, "https://gateway.example.test")
	handler, service, cookie := newGatewayWorkspaceSourceTestHandlerWithOrigin(t, codespaceUUID, policy)

	workspaceOrigin := "https://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test"
	request := httptest.NewRequest(http.MethodGet, "http://"+gatewayTestUUID32(codespaceUUID)+".gateway.example.test/w/"+codespaceUUID+"/", nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", workspaceOrigin)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("workspace status = %d body=%s", response.Code, response.Body.String())
	}
	if calls := service.revalidateCallCount(); calls != 1 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}

	rejectedHandler, rejectedService, rejectedCookie := newGatewayWorkspaceSourceTestHandlerWithOrigin(t, codespaceUUID, policy)
	rejectedRequest := httptest.NewRequest(http.MethodGet, "http://"+gatewayTestUUID32(codespaceUUID)+".gateway.example.test/w/"+codespaceUUID+"/", nil)
	rejectedRequest.AddCookie(rejectedCookie)
	rejectedRequest.Header.Set("Origin", "http://"+gatewayTestUUID32(codespaceUUID)+".gateway.example.test")
	rejectedResponse := httptest.NewRecorder()
	rejectedHandler.ServeHTTP(rejectedResponse, rejectedRequest)
	if rejectedResponse.Code != http.StatusForbidden {
		t.Fatalf("rejected workspace status = %d", rejectedResponse.Code)
	}
	if calls := rejectedService.revalidateCallCount(); calls != 0 {
		t.Fatalf("rejected revalidate rpc calls = %d", calls)
	}
}

func TestGatewayWorkspaceUsesHostBindingWithRootCompatPath(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler, service, cookie := newGatewayWorkspaceSourceTestHandlerWithOrigin(t, codespaceUUID, policy)

	target := "http://" + gatewayTestUUID32(codespaceUUID) + ".gateway.example.test/w/"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", "http://"+gatewayTestUUID32(codespaceUUID)+".gateway.example.test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("workspace status = %d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode workspace response: %v", err)
	}
	if payload["codespace_uuid"] != codespaceUUID || payload["endpoint_id"] != "workspace" {
		t.Fatalf("workspace payload = %#v", payload)
	}
	if calls := service.revalidateCallCount(); calls != 1 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}
}

func TestGatewayWorkspaceRejectsHostBindingMismatchWithoutRevalidate(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	policy := mustGatewayOriginPolicy(t, "http://gateway.example.test")
	handler, service, cookie := newGatewayWorkspaceSourceTestHandlerWithOrigin(t, codespaceUUID, policy)

	request := httptest.NewRequest(
		http.MethodGet,
		"http://web-"+gatewayTestUUID32(codespaceUUID)+".gateway.example.test/w/"+codespaceUUID+"/",
		nil,
	)
	request.AddCookie(cookie)
	request.Header.Set("Origin", "http://web-"+gatewayTestUUID32(codespaceUUID)+".gateway.example.test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("workspace host mismatch status = %d", response.Code)
	}
	if calls := service.revalidateCallCount(); calls != 0 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}
}

func TestGatewayWorkspaceConcurrentMissSharesRevalidate(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
		revalidateStarted: started,
		revalidateRelease: release,
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane)
	cookie := openGatewaySession(t, handler)

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- serveGatewayWorkspaceRequest(handler, "/w/"+codespaceUUID+"/", cookie)
	}()
	<-started

	secondDone := make(chan int, 1)
	go func() {
		secondDone <- serveGatewayWorkspaceRequest(handler, "/w/"+codespaceUUID+"/", cookie)
	}()
	select {
	case <-started:
		release <- struct{}{}
		release <- struct{}{}
		t.Fatalf("second request started a duplicate revalidate rpc")
	case <-time.After(25 * time.Millisecond):
	}

	release <- struct{}{}
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("first workspace status = %d", status)
	}
	if status := <-secondDone; status != http.StatusOK {
		t.Fatalf("second workspace status = %d", status)
	}
	if calls := service.revalidateCallCount(); calls != 1 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}
}

func TestGatewayWorkspaceValidationLimitReturnsUnavailable(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	started := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
		revalidateStarted: started,
		revalidateRelease: release,
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(
		newProcessHealth(),
		newGatewaySessionRegistry(),
		newGatewayAccessController(gatewayAccessConfig{
			allowedTTL:                      time.Second,
			maxInflightTotal:                4,
			maxInflightPerSession:           4,
			publicMaxConnectionsPerEndpoint: 4,
			publicMaxConnectionsPerIP:       4,
			validationMaxInflight:           1,
		}),
		controlPlane,
	)
	cookie := openGatewaySession(t, handler)

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- serveGatewayWorkspaceRequest(handler, "/w/"+codespaceUUID+"/", cookie)
	}()
	<-started

	publicStatus := serveGatewayPublicRequest(handler, "/p/"+codespaceUUID+"/web/", "198.51.100.10:1000", "")
	if publicStatus != http.StatusServiceUnavailable {
		t.Fatalf("public endpoint status while revalidate in flight = %d", publicStatus)
	}
	release <- struct{}{}
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("workspace status = %d", status)
	}
	if calls := service.publicEndpointCallCount(); calls != 0 {
		t.Fatalf("public endpoint rpc calls = %d", calls)
	}
}

func TestGatewayWorkspaceUsesPerSessionInflightLimit(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	started := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
		revalidateStarted: started,
		revalidateRelease: release,
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(
		newProcessHealth(),
		newGatewaySessionRegistry(),
		newGatewayAccessController(gatewayAccessConfig{
			allowedTTL:                      time.Second,
			maxInflightTotal:                4,
			maxInflightPerSession:           1,
			publicMaxConnectionsPerEndpoint: 4,
			publicMaxConnectionsPerIP:       4,
			validationMaxInflight:           4,
		}),
		controlPlane,
	)
	cookie := openGatewaySession(t, handler)

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- serveGatewayWorkspaceRequest(handler, "/w/"+codespaceUUID+"/", cookie)
	}()
	<-started

	if status := serveGatewayWorkspaceRequest(handler, "/w/"+codespaceUUID+"/", cookie); status != http.StatusTooManyRequests {
		t.Fatalf("workspace status while session capacity full = %d", status)
	}
	release <- struct{}{}
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("first workspace status = %d", status)
	}
	if calls := service.revalidateCallCount(); calls != 1 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}
}

func TestGatewayWorkspaceAndPublicAuthorizationKeysAreIsolated(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "web",
				},
			},
		},
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{
				Allowed: &codespacev1.PublicEndpointAllowed{},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(newProcessHealth(), newGatewaySessionRegistry(), newTestGatewayAccess(), controlPlane)
	cookie := openGatewaySession(t, handler)

	if status := serveGatewayPublicRequest(handler, "/p/"+codespaceUUID+"/web/", "198.51.100.10:1000", ""); status != http.StatusOK {
		t.Fatalf("public endpoint status = %d", status)
	}
	if status := serveGatewayWorkspaceRequest(handler, "/w/"+codespaceUUID+"/e/web/", cookie); status != http.StatusOK {
		t.Fatalf("workspace endpoint status = %d", status)
	}
	if calls := service.publicEndpointCallCount(); calls != 1 {
		t.Fatalf("public endpoint rpc calls = %d", calls)
	}
	if calls := service.revalidateCallCount(); calls != 1 {
		t.Fatalf("revalidate rpc calls = %d", calls)
	}
}

func TestGatewayPublicEndpointLimitUsesTCPPeerIP(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	service := &gatewayManagerService{
		publicEndpointStarted: started,
		publicEndpointRelease: release,
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{
				Allowed: &codespacev1.PublicEndpointAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(
		newProcessHealth(),
		newGatewaySessionRegistry(),
		newGatewayAccessController(gatewayAccessConfig{
			allowedTTL:                      time.Second,
			maxInflightTotal:                2,
			maxInflightPerSession:           2,
			publicMaxConnectionsPerEndpoint: 2,
			publicMaxConnectionsPerIP:       1,
			validationMaxInflight:           1,
		}),
		controlPlane,
	)

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- serveGatewayPublicRequest(handler, "/p/codespace-uuid/web/", "198.51.100.10:1000", "")
	}()
	<-started

	status := serveGatewayPublicRequest(handler, "/p/codespace-uuid/web/", "198.51.100.10:2000", "203.0.113.44")
	if status != http.StatusTooManyRequests {
		t.Fatalf("same peer public endpoint status = %d", status)
	}
	release <- struct{}{}
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("first public endpoint status = %d", status)
	}
}

func TestGatewayPublicEndpointValidationLimitReturnsUnavailable(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	service := &gatewayManagerService{
		publicEndpointStarted: started,
		publicEndpointRelease: release,
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{
				Allowed: &codespacev1.PublicEndpointAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(
		newProcessHealth(),
		newGatewaySessionRegistry(),
		newGatewayAccessController(gatewayAccessConfig{
			allowedTTL:                      time.Second,
			maxInflightTotal:                4,
			maxInflightPerSession:           4,
			publicMaxConnectionsPerEndpoint: 4,
			publicMaxConnectionsPerIP:       4,
			validationMaxInflight:           1,
		}),
		controlPlane,
	)

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- serveGatewayPublicRequest(handler, "/p/codespace-a/web/", "198.51.100.10:1000", "")
	}()
	<-started

	status := serveGatewayPublicRequest(handler, "/p/codespace-b/web/", "198.51.100.11:1000", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("validation limited public endpoint status = %d", status)
	}
	release <- struct{}{}
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("first public endpoint status = %d", status)
	}
	if calls := service.publicEndpointCallCount(); calls != 1 {
		t.Fatalf("public endpoint rpc calls = %d", calls)
	}
}

func TestGatewayPublicEndpointConcurrentMissSharesValidation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	service := &gatewayManagerService{
		publicEndpointStarted: started,
		publicEndpointRelease: release,
		publicEndpointResponse: &codespacev1.ValidatePublicEndpointResponse{
			Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{
				Allowed: &codespacev1.PublicEndpointAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newGatewayHandler(
		newProcessHealth(),
		newGatewaySessionRegistry(),
		newGatewayAccessController(gatewayAccessConfig{
			allowedTTL:                      time.Second,
			maxInflightTotal:                4,
			maxInflightPerSession:           4,
			publicMaxConnectionsPerEndpoint: 4,
			publicMaxConnectionsPerIP:       4,
			validationMaxInflight:           2,
		}),
		controlPlane,
	)

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- serveGatewayPublicRequest(handler, "/p/codespace-uuid/web/", "198.51.100.10:1000", "")
	}()
	<-started

	secondDone := make(chan int, 1)
	go func() {
		secondDone <- serveGatewayPublicRequest(handler, "/p/codespace-uuid/web/", "198.51.100.11:1000", "")
	}()
	select {
	case <-started:
		release <- struct{}{}
		release <- struct{}{}
		t.Fatalf("second request started a duplicate public endpoint rpc")
	case <-time.After(25 * time.Millisecond):
	}

	release <- struct{}{}
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("first public endpoint status = %d", status)
	}
	if status := <-secondDone; status != http.StatusOK {
		t.Fatalf("second public endpoint status = %d", status)
	}
	if calls := service.publicEndpointCallCount(); calls != 1 {
		t.Fatalf("public endpoint rpc calls = %d", calls)
	}
}

func newTestGatewayAccess() *gatewayAccessController {
	return newGatewayAccessController(gatewayAccessConfig{
		allowedTTL:                      time.Second,
		maxInflightTotal:                4096,
		maxInflightPerSession:           32,
		publicMaxConnectionsPerEndpoint: 64,
		publicMaxConnectionsPerIP:       16,
		validationMaxInflight:           128,
	})
}

func serveGatewayPublicRequest(handler http.Handler, path string, remoteAddr string, forwardedFor string) int {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		request.Header.Set("X-Forwarded-For", forwardedFor)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code
}

func serveGatewayWorkspaceRequest(handler http.Handler, path string, cookie *http.Cookie) int {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code
}

func setGatewayBrowserNavigationHeaders(header http.Header) {
	header.Set("Accept", "text/html")
	header.Set("Sec-Fetch-Site", "none")
	header.Set("Sec-Fetch-Mode", "navigate")
	header.Set("Sec-Fetch-Dest", "document")
}

func openGatewaySession(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/open?code=open-code", nil))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("open status = %d", response.Code)
	}
	assertGatewayOpenHeaders(t, response)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != gatewaySessionCookieName || cookies[0].Value == "" {
		t.Fatalf("session cookies = %#v", cookies)
	}
	return cookies[0]
}

func openGatewaySessionWithOrigin(
	t *testing.T,
	handler http.Handler,
	codespaceUUID string,
	originPolicy gatewayOriginPolicy,
) *http.Cookie {
	t.Helper()

	if originPolicy.domain == "" {
		return openGatewaySession(t, handler)
	}
	target := originPolicy.scheme + "://" + gatewayTestUUID32(codespaceUUID) + "." + originPolicy.domain
	if originPolicy.port != defaultGatewayProxyPort(originPolicy.scheme) {
		target += ":" + originPolicy.port
	}
	target += "/.gitea-codespace/open?code=open-code"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("open status = %d body=%s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/" {
		t.Fatalf("open redirect = %q", location)
	}
	assertGatewayOpenHeaders(t, response)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != gatewaySessionCookieNameForPolicy(originPolicy) || cookies[0].Value == "" {
		t.Fatalf("session cookies = %#v", cookies)
	}
	return cookies[0]
}

func gatewaySessionCookieNameForPolicy(originPolicy gatewayOriginPolicy) string {
	if strings.EqualFold(originPolicy.scheme, "https") {
		return gatewaySecureSessionCookieName
	}
	return gatewaySessionCookieName
}

func newGatewayWorkspaceSourceTestHandler(
	t *testing.T,
	codespaceUUID string,
) (http.Handler, *gatewayManagerService, *http.Cookie) {
	t.Helper()

	return newGatewayWorkspaceSourceTestHandlerWithOrigin(t, codespaceUUID, gatewayOriginPolicy{})
}

func newGatewayWorkspaceSourceTestHandlerWithOrigin(
	t *testing.T,
	codespaceUUID string,
	originPolicy gatewayOriginPolicy,
) (http.Handler, *gatewayManagerService, *http.Cookie) {
	t.Helper()

	service := &gatewayManagerService{
		openTokenResponse: &codespacev1.ValidateOpenTokenResponse{
			Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{
				Allowed: &codespacev1.OpenTokenBinding{
					UserId:        42,
					CodespaceUuid: codespaceUUID,
					EndpointId:    "workspace",
				},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	t.Cleanup(closeServer)
	handler := newGatewayHandlerWithOrigin(
		newProcessHealth(),
		newGatewaySessionRegistry(),
		newTestGatewayAccess(),
		controlPlane,
		originPolicy,
	)
	return handler, service, openGatewaySessionWithOrigin(t, handler, codespaceUUID, originPolicy)
}

func serveGatewayOpenRequest(handler http.Handler) int {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/open?code=open-code", nil))
	return response.Code
}

func assertGatewayOpenHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()

	if value := response.Header().Get("Cache-Control"); value != "no-store" {
		t.Fatalf("cache control = %q", value)
	}
	if value := response.Header().Get("Referrer-Policy"); value != "no-referrer" {
		t.Fatalf("referrer policy = %q", value)
	}
}

func mustGatewayOriginPolicy(t *testing.T, gatewayURL string) gatewayOriginPolicy {
	t.Helper()

	policy, err := newGatewayOriginPolicy(gatewayURL)
	if err != nil {
		t.Fatalf("gateway origin policy: %v", err)
	}
	return policy
}

func gatewayTestUUID32(codespaceUUID string) string {
	return strings.ReplaceAll(codespaceUUID, "-", "")
}
