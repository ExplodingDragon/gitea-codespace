// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	gatewayLogHeaderLimit = 256
	gatewayLogPathLimit   = 512
)

type gatewayUpstreamError struct {
	stage  string
	target runtimeSnapshot
	err    error
}

func (e *gatewayUpstreamError) Error() string { return e.err.Error() }
func (e *gatewayUpstreamError) Unwrap() error { return e.err }

func wrapGatewayUpstreamError(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &gatewayUpstreamError{stage: stage, err: err}
}

func wrapGatewayUpstreamTargetError(stage string, target runtimeSnapshot, err error) error {
	if err == nil {
		return nil
	}
	return &gatewayUpstreamError{stage: stage, target: target, err: err}
}

func gatewayUpstreamStage(err error, fallback string) string {
	var upstreamErr *gatewayUpstreamError
	if errors.As(err, &upstreamErr) && upstreamErr.stage != "" {
		return upstreamErr.stage
	}
	return fallback
}

func gatewayUpstreamTarget(err error) (runtimeSnapshot, bool) {
	var upstreamErr *gatewayUpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.target.RuntimeUUID == "" {
		return runtimeSnapshot{}, false
	}
	return upstreamErr.target, true
}

func gatewayLog(ctx context.Context, level slog.Level, event string, attrs ...slog.Attr) {
	if ctx == nil {
		ctx = context.Background()
	}
	attrs = append([]slog.Attr{slog.String("event", event)}, attrs...)
	slog.LogAttrs(ctx, level, event, attrs...)
}

func gatewayRequestLogAttrs(request *http.Request) []slog.Attr {
	if request == nil {
		return nil
	}
	protocol := "http"
	if isGatewayWebSocketRequest(request) {
		protocol = "websocket"
	}
	path := "/"
	if request.URL != nil && request.URL.EscapedPath() != "" {
		path = request.URL.EscapedPath()
	}
	clientIP := gatewayPeerIP(request)
	attrs := []slog.Attr{
		slog.String("client_ip", clientIP),
		slog.String("protocol", protocol),
		slog.String("method", request.Method),
		slog.String("request_host", boundedGatewayLogValue(request.Host, gatewayLogHeaderLimit)),
		slog.String("request_path", boundedGatewayLogValue(path, gatewayLogPathLimit)),
	}
	if peer, ok := gatewayRemoteIP(request.RemoteAddr); ok && peer.String() != clientIP {
		attrs = append(attrs, slog.String("peer_ip", peer.String()))
	}
	return attrs
}

func gatewayProxyLogAttrs(request *http.Request, route endpointRoute, upstreamPath string, proxyContext gatewayProxyRequestContext) []slog.Attr {
	attrs := gatewayRequestLogAttrs(request)
	attrs = append(attrs,
		slog.String("upstream_path", boundedGatewayLogValue(upstreamPath, gatewayLogPathLimit)),
		slog.String("access", proxyContext.access),
	)
	if proxyContext.userID > 0 {
		attrs = append(attrs, slog.Int64("user_id", proxyContext.userID))
	}
	return append(attrs, gatewayRouteLogAttrs(route)...)
}

func gatewayRouteLogAttrs(route endpointRoute) []slog.Attr {
	attrs := gatewayRuntimeLogAttrs(runtimeSnapshot{
		SiteUID:       route.SiteUID,
		ResourceUID:   route.ResourceUID,
		RuntimeUUID:   route.CodespaceUUID,
		PodUID:        route.PodUID,
		AgentAddress:  route.AgentAddress,
		TargetVersion: route.TargetVersion,
	})
	return append(attrs,
		slog.String("endpoint_id", route.EndpointID),
		slog.String("endpoint_label", route.Label),
		slog.Bool("public", route.Public),
	)
}

func gatewayRuntimeLogAttrs(runtime runtimeSnapshot) []slog.Attr {
	return []slog.Attr{
		slog.String("site_uid", runtime.SiteUID),
		slog.String("resource_uid", runtime.ResourceUID),
		slog.String("codespace_uuid", runtime.RuntimeUUID),
		slog.String("pod_uid", runtime.PodUID),
		slog.String("agent_address", runtime.AgentAddress),
		slog.Int64("target_version", runtime.TargetVersion),
	}
}

func gatewaySSHLogAttrs(auth gatewaySSHAuthContext, runtime runtimeSnapshot) []slog.Attr {
	if runtime.SiteUID == "" {
		runtime.SiteUID = auth.siteUID
	}
	if runtime.RuntimeUUID == "" {
		runtime.RuntimeUUID = auth.codespaceUUID
	}
	attrs := []slog.Attr{
		slog.String("client_ip", auth.clientIP),
		slog.String("protocol", "ssh"),
		slog.Int64("user_id", auth.userID),
	}
	return append(attrs, gatewayRuntimeLogAttrs(runtime)...)
}

func gatewaySourceRejectionLogAttrs(request *http.Request, policy originPolicy) []slog.Attr {
	attrs := gatewayRequestLogAttrs(request)
	expectedOrigin, _ := gatewayRequestOrigin(request, policy)
	attrs = append(attrs, slog.String("expected_origin", boundedGatewayLogValue(expectedOrigin, gatewayLogHeaderLimit)))
	origins := request.Header.Values("Origin")
	attrs = append(attrs, slog.Int("origin_count", len(origins)))
	if len(origins) == 1 {
		origin := "invalid"
		if canonical, ok := canonicalGatewayOrigin(origins[0]); ok {
			origin = canonical
		}
		attrs = append(attrs, slog.String("origin", origin))
	}
	for _, header := range []string{"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest"} {
		if value := strings.TrimSpace(request.Header.Get(header)); value != "" {
			attrs = append(attrs, slog.String(strings.ToLower(strings.ReplaceAll(header, "-", "_")), boundedGatewayLogValue(value, gatewayLogHeaderLimit)))
		}
	}
	return attrs
}

func gatewaySourceRejectionReason(request *http.Request, policy originPolicy) string {
	if _, ok := gatewayRequestOrigin(request, policy); !ok {
		return "invalid_request_host"
	}
	origins := request.Header.Values("Origin")
	if len(origins) > 1 {
		return "multiple_origins"
	}
	if len(origins) == 1 {
		if _, ok := canonicalGatewayOrigin(origins[0]); !ok {
			return "invalid_origin"
		}
		return "origin_mismatch"
	}
	if isGatewayWebSocketRequest(request) {
		return "missing_origin"
	}
	if request.Header.Get("Sec-Fetch-Site") != "" || request.Header.Get("Sec-Fetch-Mode") != "" || request.Header.Get("Sec-Fetch-Dest") != "" {
		return "fetch_metadata_not_allowed"
	}
	return "missing_source_signal"
}

func boundedGatewayLogValue(value string, limit int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if limit <= 0 || len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "..."
}
