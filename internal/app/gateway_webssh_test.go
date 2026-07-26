// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
)

func TestGatewayWorkspaceFallsBackToBuiltInWebSSHPage(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	handler, _, cookie := newGatewayWebSSHTestHandler(t, codespaceUUID, nil, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/w/"+codespaceUUID+"/", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("web ssh page status = %d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Fatalf("web ssh page content-type = %q", contentType)
	}
	csp := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Fatalf("web ssh page csp = %q", csp)
	}
	if !strings.Contains(response.Body.String(), "Workspace terminal") {
		t.Fatalf("web ssh page body = %q", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Codespace") || !strings.Contains(response.Body.String(), `id="status"`) {
		t.Fatalf("web ssh page does not include workspace chrome: %q", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "/xterm.css") ||
		!strings.Contains(response.Body.String(), "/xterm.js") ||
		!strings.Contains(response.Body.String(), "/xterm-addon-fit.js") {
		t.Fatalf("web ssh page does not load xterm assets: %q", response.Body.String())
	}
}

func TestGatewayWorkspaceBuiltInWebSSHServesXtermAssets(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	handler, _, cookie := newGatewayWebSSHTestHandler(t, codespaceUUID, nil, nil, nil)
	for _, tc := range []struct {
		path        string
		contentType string
		contains    string
	}{
		{
			path:        "/w/" + codespaceUUID + "/.gitea-codespace/assets/xterm.js",
			contentType: "application/javascript",
			contains:    "Terminal",
		},
		{
			path:        "/w/" + codespaceUUID + "/.gitea-codespace/assets/xterm.css",
			contentType: "text/css",
			contains:    ".xterm",
		},
		{
			path:        "/w/" + codespaceUUID + "/.gitea-codespace/assets/xterm-addon-fit.js",
			contentType: "application/javascript",
			contains:    "FitAddon",
		},
	} {
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", tc.path, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Header().Get("Content-Type"), tc.contentType) {
			t.Fatalf("%s content-type = %q", tc.path, response.Header().Get("Content-Type"))
		}
		if !strings.Contains(response.Body.String(), tc.contains) {
			t.Fatalf("%s body does not contain %q", tc.path, tc.contains)
		}
	}
}

func TestGatewayWorkspaceBuiltInWebSSHConnectsWorkspaceCommand(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	store := NewCodespaceStateStore(t.TempDir())
	saveGatewayWebSSHMetadata(t, store, codespaceUUID)
	backend := newTestWorkspaceCommandBackend("shell ready\n")
	backend.block = true
	handler, _, cookie := newGatewayWebSSHTestHandler(t, codespaceUUID, store, backend, nil)
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	headers := http.Header{}
	headers.Add("Cookie", cookie.String())
	headers.Set("Origin", gateway.URL)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/w/"+codespaceUUID+"/.gitea-codespace/terminal", headers)
	if err != nil {
		if response != nil {
			t.Fatalf("dial web ssh status = %d error = %v", response.StatusCode, err)
		}
		t.Fatalf("dial web ssh: %v", err)
	}
	defer conn.Close()

	messageType, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ready message: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(message), `"ready"`) {
		t.Fatalf("ready message type=%d payload=%s", messageType, message)
	}
	messageType, message, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read terminal output: %v", err)
	}
	if messageType != websocket.BinaryMessage || string(message) != "shell ready\n" {
		t.Fatalf("terminal output type=%d payload=%q", messageType, message)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":101,"rows":37}`)); err != nil {
		t.Fatalf("write resize message: %v", err)
	}
	if resize := waitTestWorkspaceResize(t, backend, 101, 37); resize.cols != 101 || resize.rows != 37 {
		t.Fatalf("terminal resize = %#v", resize)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, make([]byte, gatewayWebSSHInputLimit+1)); err != nil {
		t.Fatalf("write oversized input: %v", err)
	}
	messageType, message, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read protocol error: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(message), `"protocol_error"`) {
		t.Fatalf("protocol error message type=%d payload=%s", messageType, message)
	}
}

func TestGatewayWorkspaceBuiltInWebSSHForwardsTerminalInput(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	store := NewCodespaceStateStore(t.TempDir())
	saveGatewayWebSSHMetadata(t, store, codespaceUUID)
	backend := newTestWorkspaceCommandBackend("")
	backend.block = true
	backend.captureStdin()
	handler, _, cookie := newGatewayWebSSHTestHandler(t, codespaceUUID, store, backend, nil)
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	headers := http.Header{}
	headers.Add("Cookie", cookie.String())
	headers.Set("Origin", gateway.URL)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/w/"+codespaceUUID+"/.gitea-codespace/terminal", headers)
	if err != nil {
		if response != nil {
			t.Fatalf("dial web ssh status = %d error = %v", response.StatusCode, err)
		}
		t.Fatalf("dial web ssh: %v", err)
	}
	defer conn.Close()

	messageType, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ready message: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(message), `"ready"`) {
		t.Fatalf("ready message type=%d payload=%s", messageType, message)
	}
	input := []byte("ab\x7f\x1b[3~\t\r")
	if err := conn.WriteMessage(websocket.BinaryMessage, input); err != nil {
		t.Fatalf("write terminal input: %v", err)
	}
	select {
	case got := <-backend.stdin:
		if string(got) != string(input) {
			t.Fatalf("terminal input = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for terminal input")
	}
}

func TestGatewayWorkspaceBuiltInWebSSHReportsSessionUnavailable(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	store := NewCodespaceStateStore(t.TempDir())
	handler, _, cookie := newGatewayWebSSHTestHandler(t, codespaceUUID, store, newTestWorkspaceCommandBackend(""), nil)
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	headers := http.Header{}
	headers.Add("Cookie", cookie.String())
	headers.Set("Origin", gateway.URL)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/w/"+codespaceUUID+"/.gitea-codespace/terminal", headers)
	if err != nil {
		if response != nil {
			t.Fatalf("dial web ssh status = %d error = %v", response.StatusCode, err)
		}
		t.Fatalf("dial web ssh: %v", err)
	}
	defer conn.Close()

	messageType, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read session unavailable error: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(message), `"session_unavailable"`) {
		t.Fatalf("session unavailable error type=%d payload=%s", messageType, message)
	}
}

func TestGatewayWorkspaceBuiltInWebSSHReportsExit(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	store := NewCodespaceStateStore(t.TempDir())
	saveGatewayWebSSHMetadata(t, store, codespaceUUID)
	backend := newTestWorkspaceCommandBackend("")
	backend.exitStatus = 7
	handler, _, cookie := newGatewayWebSSHTestHandler(t, codespaceUUID, store, backend, nil)
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	headers := http.Header{}
	headers.Add("Cookie", cookie.String())
	headers.Set("Origin", gateway.URL)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/w/"+codespaceUUID+"/.gitea-codespace/terminal", headers)
	if err != nil {
		if response != nil {
			t.Fatalf("dial web ssh status = %d error = %v", response.StatusCode, err)
		}
		t.Fatalf("dial web ssh: %v", err)
	}
	defer conn.Close()

	messageType, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ready message: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(message), `"ready"`) {
		t.Fatalf("ready message type=%d payload=%s", messageType, message)
	}
	messageType, message, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read exit message: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(message), `"exit"`) || !strings.Contains(string(message), `"code":7`) {
		t.Fatalf("exit message type=%d payload=%s", messageType, message)
	}
}

func TestGatewayWorkspaceRouteOverridesBuiltInWebSSH(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, "runtime workspace")
	}))
	defer upstream.Close()

	routes := newGatewayRouteStore()
	routes.SetWorkspaceTerminal(newGatewayWorkspaceTerminal(nil, nil))
	if err := routes.Put(gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     "workspace",
		upstreamScheme: "http",
		upstreamHost:   strings.TrimPrefix(upstream.URL, "http://"),
	}); err != nil {
		t.Fatalf("put route: %v", err)
	}
	handler, _, cookie := newGatewayWebSSHTestHandler(t, codespaceUUID, nil, nil, routes)
	request := httptest.NewRequest(http.MethodGet, "/w/"+codespaceUUID+"/", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("workspace route status = %d body=%s", response.Code, response.Body.String())
	}
	if response.Body.String() != "runtime workspace" {
		t.Fatalf("workspace route body = %q", response.Body.String())
	}
}

func newGatewayWebSSHTestHandler(
	t *testing.T,
	codespaceUUID string,
	store *CodespaceStateStore,
	backend gatewayWorkspaceCommandBackend,
	routes *gatewayRouteStore,
) (http.Handler, *gatewayManagerService, *http.Cookie) {
	t.Helper()

	if routes == nil {
		routes = newGatewayRouteStore()
	}
	if backend == nil {
		backend = newTestWorkspaceCommandBackend("shell ready\n")
	}
	routes.SetWorkspaceTerminal(newGatewayWorkspaceTerminal(store, backend))
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
	handler := newGatewayHandlerWithOriginAndBrowserAuth(
		newProcessHealth(),
		newGatewaySessionRegistry(),
		newTestGatewayAccess(),
		controlPlane,
		gatewayOriginPolicy{},
		nil,
		routes,
	)
	return handler, service, openGatewaySession(t, handler)
}

func saveGatewayWebSSHMetadata(t *testing.T, store *CodespaceStateStore, codespaceUUID string) {
	t.Helper()

	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 1,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       100,
			LastUpdateUnix:    100,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}
	saveGatewayWorkspaceIdentityForTest(t, store, codespaceUUID)
}
