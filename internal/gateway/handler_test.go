// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type gatewayHandlerTestControlPlane struct{}

func (gatewayHandlerTestControlPlane) ValidateOpenToken(context.Context, string, string, string) (gatewayOpenTokenDecision, error) {
	return gatewayOpenTokenDecision{}, nil
}

func (gatewayHandlerTestControlPlane) ValidatePublicEndpoint(context.Context, string, string) (gatewayAccessDecision, error) {
	return gatewayAccessDecision{Allowed: true}, nil
}

func (gatewayHandlerTestControlPlane) VerifySSHPublicKey(context.Context, string, []byte) (gatewaySSHAuthDecision, error) {
	return gatewaySSHAuthDecision{}, nil
}

func (gatewayHandlerTestControlPlane) RevalidateEndpointSession(context.Context, int64, string, string) (gatewayAccessDecision, error) {
	return gatewayAccessDecision{}, nil
}

func (gatewayHandlerTestControlPlane) RevalidateSSHSession(context.Context, int64, string) (gatewayAccessDecision, error) {
	return gatewayAccessDecision{}, nil
}

type gatewayHandlerTestBackend struct {
	address string
}

func (gatewayHandlerTestBackend) OpenWorkspaceCommand(context.Context, WorkspaceCommandRequest) (WorkspaceCommandSession, error) {
	return nil, fmt.Errorf("command is not available")
}

func (gatewayHandlerTestBackend) OpenWorkspaceSFTP(context.Context, WorkspaceSFTPRequest) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("SFTP is not available")
}

func (b gatewayHandlerTestBackend) OpenWorkspaceTCP(ctx context.Context, _ string, _ uint32) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", b.address)
}

func (b gatewayHandlerTestBackend) OpenWorkspaceEndpoint(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", b.address)
}

func TestGatewayPublicEndpointRootProxiesWebSocket(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/socket" || !isGatewayWebSocketRequest(request) {
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
	routes := NewRouteStore(gatewayHandlerTestBackend{address: upstreamAddress})
	t.Cleanup(routes.Close)
	const runtimeUUID = "11111111-1111-4111-8111-111111111111"
	if err := routes.Put(EndpointRoute{
		GiteaWebURL:   "https://gitea.example.test",
		CodespaceUUID: runtimeUUID,
		EndpointID:    "app-3000",
		Label:         "Application",
		Public:        true,
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := NewOriginPolicy("http://gateway.example.test")
	if err != nil {
		t.Fatal(err)
	}
	access := NewAccessController(AccessConfig{
		AllowedTTL:                      time.Minute,
		StreamRevalidateInterval:        time.Hour,
		MaxInflightTotal:                8,
		MaxInflightPerSession:           4,
		PublicMaxConnectionsPerEndpoint: 4,
		PublicMaxConnectionsPerIP:       4,
		ValidationMaxInflight:           4,
	})
	gateway := httptest.NewServer(NewHandler(nil, nil, access, gatewayHandlerTestControlPlane{}, policy, nil, routes))
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodGet, gateway.URL+"/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "app-3000-11111111111141118111111111111111.gateway.example.test"
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
