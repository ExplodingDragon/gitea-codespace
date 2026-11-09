// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGatewayLogFieldsIdentifyRouteWithoutCredentials(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://workspace.example.test/project/file?code=open-secret&token=query-secret", nil)
	request.RemoteAddr = "10.0.0.2:12345"
	request.Header.Set("Authorization", "Bearer authorization-secret")
	request.Header.Set("Cookie", "gitea_codespace_session=session-secret")
	request = request.WithContext(context.WithValue(request.Context(), gatewayClientIPContextKey{}, "203.0.113.10"))
	route := endpointRoute{
		SiteUID: "site-1", ResourceUID: "resource-1", CodespaceUUID: "codespace-1",
		EndpointID: "workspace", Label: "Web IDE", PodUID: "pod-1", AgentAddress: "agent:8444", TargetVersion: 9,
	}
	attrs := gatewayProxyLogAttrs(request, route, "/project/file", gatewayProxyRequestContext{access: "authenticated", userID: 42})

	var output bytes.Buffer
	handler := slog.NewJSONHandler(&output, nil)
	record := slog.NewRecord(time.Now(), slog.LevelError, "gateway_proxy_failed", 0)
	record.AddAttrs(attrs...)
	require.NoError(t, handler.Handle(t.Context(), record))
	var fields map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &fields))
	require.Equal(t, "203.0.113.10", fields["client_ip"])
	require.Equal(t, "10.0.0.2", fields["peer_ip"])
	require.Equal(t, "resource-1", fields["resource_uid"])
	require.Equal(t, "agent:8444", fields["agent_address"])
	require.Equal(t, float64(9), fields["target_version"])
	require.Equal(t, "/project/file", fields["request_path"])
	require.Equal(t, "/project/file", fields["upstream_path"])
	for _, secret := range []string{"open-secret", "query-secret", "authorization-secret", "session-secret"} {
		require.NotContains(t, output.String(), secret)
	}
}

func TestGatewaySourceRejectionFieldsAreBounded(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://workspace.example.test/api", nil)
	request.Header.Set("Origin", "not-an-origin-"+strings.Repeat("x", 1024))
	request.Header.Set("Sec-Fetch-Site", strings.Repeat("cross-site", 64))
	attrs := gatewaySourceRejectionLogAttrs(request, originPolicy{Scheme: "https", Domain: "example.test", Port: "443"})

	var output bytes.Buffer
	handler := slog.NewJSONHandler(&output, nil)
	record := slog.NewRecord(time.Now(), slog.LevelWarn, "gateway_source_rejected", 0)
	record.AddAttrs(attrs...)
	require.NoError(t, handler.Handle(t.Context(), record))
	require.NotContains(t, output.String(), strings.Repeat("x", 1024))
	require.Contains(t, output.String(), `"origin":"invalid"`)
}

func TestGatewayUpstreamErrorPreservesStageAndTarget(t *testing.T) {
	err := wrapGatewayUpstreamTargetError("agent_connect", runtimeSnapshot{
		SiteUID: "site-1", ResourceUID: "resource-1", RuntimeUUID: "codespace-1",
		PodUID: "pod-1", AgentAddress: "agent:8444", TargetVersion: 5,
	}, errors.New("connection failed"))
	require.Equal(t, "agent_connect", gatewayUpstreamStage(err, "proxy_stream"))
	target, found := gatewayUpstreamTarget(err)
	require.True(t, found)
	require.Equal(t, "resource-1", target.ResourceUID)
	require.Equal(t, "agent:8444", target.AgentAddress)
	require.ErrorContains(t, err, "connection failed")
}
