// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

type gatewayHandlerTestControlPlane struct {
	openDecision gatewayOpenTokenDecision
}

func (c gatewayHandlerTestControlPlane) ValidateOpenToken(context.Context, string, string, string) (gatewayOpenTokenDecision, error) {
	return c.openDecision, nil
}

func (gatewayHandlerTestControlPlane) ValidatePublicEndpoint(context.Context, string, string, string) (gatewayAccessDecision, error) {
	return gatewayAccessDecision{Allowed: true}, nil
}

func (gatewayHandlerTestControlPlane) VerifySSHPublicKey(context.Context, string, []byte) (gatewaySSHAuthDecision, error) {
	return gatewaySSHAuthDecision{}, nil
}

func (gatewayHandlerTestControlPlane) RevalidateEndpointSession(context.Context, string, int64, string, string) (gatewayAccessDecision, error) {
	return gatewayAccessDecision{}, nil
}

func (gatewayHandlerTestControlPlane) RevalidateSSHSession(context.Context, string, int64, string) (gatewayAccessDecision, error) {
	return gatewayAccessDecision{}, nil
}

type gatewayTestBackend struct {
	address       string
	endpointError error
}

func (gatewayTestBackend) OpenWorkspaceShell(context.Context, workspaceShellRequest) (workspaceCommandSession, error) {
	return nil, fmt.Errorf("shell is not available")
}

func (gatewayTestBackend) OpenWorkspaceCommand(context.Context, workspaceCommandRequest) (workspaceCommandSession, error) {
	return nil, fmt.Errorf("command is not available")
}

func (gatewayTestBackend) OpenWorkspaceSFTP(context.Context, workspaceSFTPRequest) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("SFTP is not available")
}

func (b gatewayTestBackend) OpenWorkspaceTCP(ctx context.Context, _ string, _ uint32) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", b.address)
}

func (b gatewayTestBackend) OpenWorkspaceEndpoint(ctx context.Context, _, _ string) (net.Conn, error) {
	if b.endpointError != nil {
		return nil, b.endpointError
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", b.address)
}

func TestGatewayPublicEndpointRootProxiesWebSocket(t *testing.T) {
	t.Parallel()

	const externalHost = "app-3000-11111111111141118111111111111111.gateway.example.test"
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/socket" || !isGatewayWebSocketRequest(request) || request.Host != externalHost {
			http.Error(writer, "invalid upgrade request", http.StatusBadRequest)
			return
		}
		connection, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		if buffered.Flush() != nil {
			return
		}
		payload := make([]byte, 4)
		if _, err := io.ReadFull(buffered, payload); err == nil {
			_, _ = connection.Write(payload)
		}
	}))
	defer upstream.Close()

	upstreamAddress := upstream.Listener.Addr().String()
	routes := newRouteStore(gatewayTestBackend{address: upstreamAddress})
	t.Cleanup(routes.Close)
	const runtimeUUID = "11111111-1111-4111-8111-111111111111"
	if err := routes.Put(endpointRoute{
		SiteUID:       "site-1",
		GiteaWebURL:   "https://gitea.example.test",
		CodespaceUUID: runtimeUUID,
		EndpointID:    "app-3000",
		Label:         "Application",
		Public:        true,
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := newOriginPolicy("http://gateway.example.test")
	if err != nil {
		t.Fatal(err)
	}
	access := newAccessController(accessConfig{
		AllowedTTL:                      time.Minute,
		StreamRevalidateInterval:        time.Hour,
		MaxInflightTotal:                8,
		MaxInflightPerSession:           4,
		PublicMaxConnectionsPerEndpoint: 4,
		PublicMaxConnectionsPerIP:       4,
		ValidationMaxInflight:           4,
	})
	gateway := httptest.NewServer(newHandler(nil, access, gatewayHandlerTestControlPlane{}, policy, nil, routes))
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodGet, gateway.URL+"/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = externalHost
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusSwitchingProtocols)
	}
	connection, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatal("upgraded response is not bidirectional")
	}
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4)
	if _, err := io.ReadFull(connection, payload); err != nil {
		t.Fatal(err)
	}
	if string(payload) != "ping" {
		t.Fatalf("payload = %q, want ping", payload)
	}
}

func TestGatewayEnforcesRouteSiteBinding(t *testing.T) {
	t.Parallel()

	const runtimeUUID = "11111111-1111-4111-8111-111111111111"
	routes := newRouteStore(gatewayTestBackend{})
	t.Cleanup(routes.Close)
	if err := routes.Put(endpointRoute{
		SiteUID:       "site-1",
		GiteaWebURL:   "https://gitea.example.test",
		CodespaceUUID: runtimeUUID,
		EndpointID:    runtimeendpoint.WorkspaceEndpointID,
		Label:         runtimeendpoint.WorkspaceEndpointLabel,
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := newOriginPolicy("https://gateway.example.test")
	if err != nil {
		t.Fatal(err)
	}
	sessions := newGatewaySessionRegistry(t)
	access := newAccessControllerFromConfig(validGatewayConfig())

	t.Run("open code for another site", func(t *testing.T) {
		control := gatewayHandlerTestControlPlane{openDecision: gatewayOpenTokenDecision{
			Allowed: true,
			Binding: openTokenBinding{SiteUID: "site-2", UserID: 1, CodespaceUUID: runtimeUUID, EndpointID: runtimeendpoint.WorkspaceEndpointID},
		}}
		request := httptest.NewRequest(http.MethodGet, "https://11111111111141118111111111111111.gateway.example.test/.gitea-codespace/open?code=secret", nil)
		response := httptest.NewRecorder()
		newHandler(sessions.sessionRegistry, access, control, policy, nil, routes).ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
		}
	})

	t.Run("browser session for another site", func(t *testing.T) {
		token, err := sessions.Create(openTokenBinding{SiteUID: "site-2", UserID: 1, CodespaceUUID: runtimeUUID, EndpointID: runtimeendpoint.WorkspaceEndpointID}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "https://11111111111141118111111111111111.gateway.example.test/", nil)
		request.AddCookie(&http.Cookie{Name: secureSessionCookieName, Value: token})
		response := httptest.NewRecorder()
		newHandler(sessions.sessionRegistry, access, gatewayHandlerTestControlPlane{}, policy, nil, routes).ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
		}
	})

	t.Run("open code without a route", func(t *testing.T) {
		const missingRuntimeUUID = "22222222-2222-4222-8222-222222222222"
		control := gatewayHandlerTestControlPlane{openDecision: gatewayOpenTokenDecision{
			Allowed: true,
			Binding: openTokenBinding{SiteUID: "site-1", UserID: 1, CodespaceUUID: missingRuntimeUUID, EndpointID: runtimeendpoint.WorkspaceEndpointID},
		}}
		request := httptest.NewRequest(http.MethodGet, "https://22222222222242228222222222222222.gateway.example.test/.gitea-codespace/open?code=secret", nil)
		response := httptest.NewRecorder()
		newHandler(sessions.sessionRegistry, access, control, policy, nil, routes).ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
		}
		for _, cookie := range response.Result().Cookies() {
			if cookie.Name == secureSessionCookieName {
				t.Fatal("gateway created a session without a runtime route")
			}
		}
	})

	t.Run("browser session after route removal", func(t *testing.T) {
		token, err := sessions.Create(openTokenBinding{SiteUID: "site-1", UserID: 1, CodespaceUUID: runtimeUUID, EndpointID: runtimeendpoint.WorkspaceEndpointID}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		routes.Delete(runtimeUUID, runtimeendpoint.WorkspaceEndpointID)
		if _, ok := sessions.Authenticate(token, runtimeUUID, runtimeendpoint.WorkspaceEndpointID, time.Now()); !ok {
			t.Fatal("route removal invalidated the browser session")
		}
		request := httptest.NewRequest(http.MethodGet, "https://11111111111141118111111111111111.gateway.example.test/", nil)
		request.AddCookie(&http.Cookie{Name: secureSessionCookieName, Value: token})
		response := httptest.NewRecorder()
		newHandler(sessions.sessionRegistry, access, gatewayHandlerTestControlPlane{}, policy, nil, routes).ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
		}
	})
}

func TestGatewayOpenCodeAllowsUnrelatedQueryParameters(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "https://gateway.example.test/open?code=secret&utm_source=gitea", nil)
	code, ok := gatewayOpenCode(request)
	if !ok || code != "secret" {
		t.Fatalf("open code = %q, ok = %v", code, ok)
	}
	request = httptest.NewRequest(http.MethodGet, "https://gateway.example.test/open?code=first&code=second", nil)
	if _, ok := gatewayOpenCode(request); ok {
		t.Fatal("duplicate open code was accepted")
	}
}

func TestGatewayProxyRevalidationRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	type result struct {
		decision gatewayAccessDecision
		err      error
	}
	results := make(chan result)
	calls := make(chan struct{}, 2)
	request := httptest.NewRequest(http.MethodGet, "https://gateway.example.test/", nil)
	proxyRequest, cancel := withGatewayProxyRevalidation(request, 10*time.Millisecond, "test revalidation", func(context.Context) (gatewayAccessDecision, error) {
		calls <- struct{}{}
		outcome := <-results
		return outcome.decision, outcome.err
	})
	defer cancel()

	results <- result{err: errors.New("temporary failure")}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("revalidation did not run")
	}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("revalidation did not retry")
	}
	select {
	case <-proxyRequest.Context().Done():
		t.Fatal("transient failure canceled the proxy request")
	default:
	}
	results <- result{decision: gatewayAccessDecision{Allowed: false}}
	select {
	case <-proxyRequest.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("explicit denial did not cancel the proxy request")
	}
}

func TestGatewayProxyClientCancellationDoesNotWriteGatewayError(t *testing.T) {
	t.Parallel()

	routes := newRouteStore(gatewayTestBackend{endpointError: context.Canceled})
	t.Cleanup(routes.Close)
	route := endpointRoute{
		SiteUID: "site-1", GiteaWebURL: "https://gitea.example.test", CodespaceUUID: "codespace-1",
		EndpointID: "app", Label: "App", Public: true,
	}
	if err := routes.Put(route); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "https://app.gateway.example.test/", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	proxyGatewayEndpoint(response, request, routes, route, "/", gatewayProxyRequestContext{externalScheme: "https", externalHost: request.Host})
	if response.Body.Len() != 0 || response.Header().Get("Content-Type") != "" {
		t.Fatalf("client cancellation produced a gateway response: %q", response.Body.String())
	}
}
