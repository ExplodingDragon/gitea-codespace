// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type gatewayOriginPolicy struct {
	domain string
	scheme string
	port   string
}

type gatewayEndpointHost struct {
	codespaceUUID string
	endpointID    string
}

func newGatewayOriginPolicy(gatewayURL string) (gatewayOriginPolicy, error) {
	parsed, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil {
		return gatewayOriginPolicy{}, fmt.Errorf("parse gateway url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return gatewayOriginPolicy{}, fmt.Errorf("gateway url scheme must be http or https")
	}
	if parsed.Host == "" {
		return gatewayOriginPolicy{}, fmt.Errorf("gateway url host is required")
	}
	domain := strings.ToLower(parsed.Hostname())
	if domain == "" {
		return gatewayOriginPolicy{}, fmt.Errorf("gateway url host is required")
	}
	port := parsed.Port()
	if port == "" {
		port = defaultGatewayProxyPort(parsed.Scheme)
	}
	return gatewayOriginPolicy{domain: domain, scheme: parsed.Scheme, port: port}, nil
}

func (p gatewayOriginPolicy) parseEndpointHost(hostValue string) (gatewayEndpointHost, bool) {
	if p.domain == "" {
		return gatewayEndpointHost{}, false
	}
	authority, ok := canonicalGatewayProxyAuthority(hostValue, p.scheme)
	if !ok {
		return gatewayEndpointHost{}, false
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil || port != p.port {
		return gatewayEndpointHost{}, false
	}
	if host == p.domain {
		return gatewayEndpointHost{}, false
	}
	suffix := "." + p.domain
	if !strings.HasSuffix(host, suffix) {
		return gatewayEndpointHost{}, false
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return gatewayEndpointHost{}, false
	}
	return parseGatewayEndpointLabel(label)
}

func (p gatewayOriginPolicy) bindingForRequest(request *http.Request) (gatewayEndpointHost, bool) {
	if p.domain == "" {
		return gatewayEndpointHost{}, false
	}
	return p.parseEndpointHost(request.Host)
}

func (p gatewayOriginPolicy) requestMatchesBinding(request *http.Request, codespaceUUID, endpointID string) bool {
	if p.domain == "" {
		return true
	}
	binding, ok := p.bindingForRequest(request)
	if !ok {
		return false
	}
	return binding.codespaceUUID == codespaceUUID && binding.endpointID == endpointID
}

func parseGatewayEndpointLabel(label string) (gatewayEndpointHost, bool) {
	if len(label) == 32 && isGatewayUUID32(label) {
		return gatewayEndpointHost{
			codespaceUUID: uuid32ToCanonical(label),
			endpointID:    "workspace",
		}, true
	}
	prefix, uuid32, ok := strings.Cut(label, "-")
	if !ok {
		return gatewayEndpointHost{}, false
	}
	lastHyphen := strings.LastIndex(label, "-")
	if lastHyphen < 1 || lastHyphen+1 >= len(label) {
		return gatewayEndpointHost{}, false
	}
	prefix = label[:lastHyphen]
	uuid32 = label[lastHyphen+1:]
	if !isGatewayEndpointID(prefix) || !isGatewayUUID32(uuid32) {
		return gatewayEndpointHost{}, false
	}
	return gatewayEndpointHost{
		codespaceUUID: uuid32ToCanonical(uuid32),
		endpointID:    prefix,
	}, true
}

func isGatewayEndpointID(value string) bool {
	if value == "workspace" || len(value) < 1 || len(value) > 30 {
		return false
	}
	for i, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		if r == '-' && i > 0 && i < len(value)-1 {
			continue
		}
		return false
	}
	return true
}

func isGatewayUUID32(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}

func uuid32ToCanonical(value string) string {
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]
}

func isGatewayAuthenticatedSourceAllowed(request *http.Request, policy gatewayOriginPolicy) bool {
	expectedOrigin, ok := gatewayRequestOrigin(request, policy)
	if !ok {
		return false
	}
	if isGatewayWebSocketRequest(request) {
		return gatewayOriginMatches(request.Header.Values("Origin"), expectedOrigin)
	}
	if gatewayMethodRequiresOrigin(request.Method) {
		return gatewayOriginMatches(request.Header.Values("Origin"), expectedOrigin)
	}
	originValues := request.Header.Values("Origin")
	if len(originValues) > 0 {
		return gatewayOriginMatches(originValues, expectedOrigin)
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(request.Header.Get("Sec-Fetch-Site")), "same-origin") {
		return true
	}
	if gatewayIsTopLevelNavigation(request) {
		return true
	}
	return gatewayHasNoFetchMetadata(request)
}

func gatewayOriginMatches(values []string, expectedOrigin string) bool {
	if len(values) != 1 {
		return false
	}
	origin, ok := canonicalGatewayOrigin(values[0])
	return ok && origin == expectedOrigin
}

func gatewayRequestOrigin(request *http.Request, policy gatewayOriginPolicy) (string, bool) {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	if request.URL != nil && (request.URL.Scheme == "http" || request.URL.Scheme == "https") {
		scheme = request.URL.Scheme
	}
	if policy.scheme != "" {
		scheme = policy.scheme
	}
	host := request.Host
	if host == "" && request.URL != nil {
		host = request.URL.Host
	}
	authority, ok := canonicalGatewayProxyAuthority(host, scheme)
	if !ok {
		return "", false
	}
	if policy.port != "" {
		_, port, err := net.SplitHostPort(authority)
		if err != nil || port != policy.port {
			return "", false
		}
	}
	return gatewayOriginFromAuthority(scheme, authority), true
}

func canonicalGatewayOrigin(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "null") || strings.Contains(value, ",") {
		return "", false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil {
		return "", false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	if parsed.User != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	authority, ok := canonicalGatewayProxyAuthority(parsed.Host, parsed.Scheme)
	if !ok {
		return "", false
	}
	return gatewayOriginFromAuthority(parsed.Scheme, authority), true
}

func gatewayOriginFromAuthority(scheme, authority string) string {
	host, port, err := net.SplitHostPort(authority)
	if err == nil && port == defaultGatewayProxyPort(scheme) {
		return scheme + "://" + host
	}
	return scheme + "://" + authority
}

func gatewayMethodRequiresOrigin(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

func isGatewayWebSocketRequest(request *http.Request) bool {
	return headerHasToken(request.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket")
}

func gatewayIsTopLevelNavigation(request *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(request.Header.Get("Sec-Fetch-Mode")), "navigate") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(request.Header.Get("Sec-Fetch-Dest")), "document") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(request.Header.Get("Sec-Fetch-Site"))) {
	case "none", "same-origin", "same-site", "cross-site":
		return true
	default:
		return false
	}
}

func gatewayHasNoFetchMetadata(request *http.Request) bool {
	return request.Header.Get("Sec-Fetch-Site") == "" &&
		request.Header.Get("Sec-Fetch-Mode") == "" &&
		request.Header.Get("Sec-Fetch-Dest") == "" &&
		request.Header.Get("Sec-Fetch-User") == ""
}

func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}
