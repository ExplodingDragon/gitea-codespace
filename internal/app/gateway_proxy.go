// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type gatewayProxyResponseContext struct {
	externalScheme string
	externalHost   string
	upstreamScheme string
	upstreamHost   string
}

type gatewayProxyRequestContext struct {
	codespaceUUID  string
	endpointID     string
	access         string
	userID         int64
	externalScheme string
	externalHost   string
}

const (
	gatewayProxyHeaderCodespaceUUID = "X-Gitea-Codespace-UUID"
	gatewayProxyHeaderEndpointID    = "X-Gitea-Codespace-Endpoint-ID"
	gatewayProxyHeaderAccess        = "X-Gitea-Codespace-Access"
	gatewayProxyHeaderUserID        = "X-Gitea-Codespace-User-ID"

	gatewaySecureSessionCookieName  = "__Host-gitea_codespace_session"
	gatewayReturnToCookieName       = "gitea_codespace_return_to"
	gatewaySecureReturnToCookieName = "__Host-gitea_codespace_return_to"
)

func prepareGatewayProxyRequest(request *http.Request, proxyContext gatewayProxyRequestContext) {
	header := request.Header
	header.Del(gatewayProxyHeaderCodespaceUUID)
	header.Del(gatewayProxyHeaderEndpointID)
	header.Del(gatewayProxyHeaderAccess)
	header.Del(gatewayProxyHeaderUserID)
	header.Del("Forwarded")
	header.Del("X-Forwarded-For")
	header.Del("X-Forwarded-Proto")
	header.Del("X-Forwarded-Host")
	rebuildGatewayProxyCookieHeader(request)

	header.Set(gatewayProxyHeaderCodespaceUUID, proxyContext.codespaceUUID)
	header.Set(gatewayProxyHeaderEndpointID, proxyContext.endpointID)
	header.Set(gatewayProxyHeaderAccess, proxyContext.access)
	if proxyContext.userID > 0 {
		header.Set(gatewayProxyHeaderUserID, formatInt64(proxyContext.userID))
	}
	header.Set("X-Forwarded-For", gatewayPeerIP(request))
	header.Set("X-Forwarded-Proto", proxyContext.externalScheme)
	header.Set("X-Forwarded-Host", firstNonEmpty(proxyContext.externalHost, request.Host))
}

func rebuildGatewayProxyCookieHeader(request *http.Request) {
	cookies := parseGatewayProxyRequestCookies(request.Header.Values("Cookie"))
	values := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if isGatewayReservedCookieName(cookie.Name) {
			continue
		}
		values = append(values, cookie.String())
	}
	if len(values) == 0 {
		request.Header.Del("Cookie")
		return
	}
	request.Header.Set("Cookie", strings.Join(values, "; "))
}

func parseGatewayProxyRequestCookies(headers []string) []*http.Cookie {
	cookies := make([]*http.Cookie, 0)
	for _, header := range headers {
		for _, part := range strings.Split(header, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if name, _, ok := strings.Cut(part, "="); !ok || strings.TrimSpace(name) == "" {
				continue
			}
			parsed, err := http.ParseCookie(part)
			if err != nil || len(parsed) != 1 {
				continue
			}
			cookies = append(cookies, parsed[0])
		}
	}
	return cookies
}

func isGatewayReservedCookieName(name string) bool {
	switch name {
	case gatewaySessionCookieName,
		gatewaySecureSessionCookieName,
		gatewayReturnToCookieName,
		gatewaySecureReturnToCookieName:
		return true
	default:
		return false
	}
}

func normalizeGatewayProxyResponse(header http.Header, proxyContext gatewayProxyResponseContext) {
	header.Del("Service-Worker-Allowed")
	rebuildGatewayProxySetCookieHeaders(header, proxyContext)
	if location := header.Get("Location"); location != "" {
		header.Set("Location", rewriteGatewayProxyLocation(location, proxyContext))
	}
}

func rebuildGatewayProxySetCookieHeaders(header http.Header, proxyContext gatewayProxyResponseContext) {
	values := header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	header.Del("Set-Cookie")
	for _, value := range values {
		cookie, ok := normalizeGatewayProxySetCookie(value, proxyContext)
		if !ok {
			continue
		}
		header.Add("Set-Cookie", cookie.String())
	}
}

func normalizeGatewayProxySetCookie(value string, proxyContext gatewayProxyResponseContext) (*http.Cookie, bool) {
	hadInvalidSameSite := hasInvalidGatewayProxySameSite(value)
	cookie, err := http.ParseSetCookie(value)
	if err != nil {
		log.Printf("drop upstream set-cookie: parse: %v", err)
		return nil, false
	}
	if isGatewayReservedCookieName(cookie.Name) {
		return nil, false
	}
	cookie.Domain = ""
	cookie.Raw = ""
	cookie.RawExpires = ""
	cookie.Unparsed = nil
	if !isValidGatewayProxyCookiePath(cookie.Path) {
		cookie.Path = "/"
	}
	if strings.HasPrefix(cookie.Name, "__Host-") {
		cookie.Path = "/"
	}
	if hadInvalidSameSite {
		cookie.SameSite = http.SameSiteLaxMode
	}
	if isGatewayProxyHTTPS(proxyContext) {
		cookie.Secure = true
	} else if !isGatewayProxyCookieAllowedOnHTTP(cookie) {
		return nil, false
	}
	if err := cookie.Valid(); err != nil {
		log.Printf("drop upstream set-cookie %q: %v", cookie.Name, err)
		return nil, false
	}
	return cookie, true
}

func hasInvalidGatewayProxySameSite(value string) bool {
	parts := strings.Split(value, ";")
	for _, attribute := range parts[1:] {
		name, attrValue, ok := strings.Cut(strings.TrimSpace(attribute), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "samesite") {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(attrValue)) {
		case "strict", "lax", "none":
			return false
		default:
			return true
		}
	}
	return false
}

func isValidGatewayProxyCookiePath(path string) bool {
	if path == "" {
		return false
	}
	if !strings.HasPrefix(path, "/") {
		return false
	}
	return !strings.ContainsFunc(path, func(r rune) bool {
		return r < 0x20 || r == 0x7f || r == ';'
	})
}

func isGatewayProxyHTTPS(proxyContext gatewayProxyResponseContext) bool {
	return strings.EqualFold(strings.TrimSpace(proxyContext.externalScheme), "https")
}

func isGatewayProxyCookieAllowedOnHTTP(cookie *http.Cookie) bool {
	if cookie.Partitioned {
		return false
	}
	if cookie.SameSite == http.SameSiteNoneMode {
		return false
	}
	if strings.HasPrefix(cookie.Name, "__Host-") || strings.HasPrefix(cookie.Name, "__Secure-") {
		return false
	}
	return true
}

func rewriteGatewayProxyLocation(location string, proxyContext gatewayProxyResponseContext) string {
	parsed, err := url.Parse(location)
	if err != nil || parsed.Host == "" {
		return location
	}
	if !sameGatewayProxyAuthority(parsed.Host, parsed.Scheme, proxyContext.upstreamHost, proxyContext.upstreamScheme) {
		return location
	}
	parsed.Scheme = proxyContext.externalScheme
	parsed.Host = proxyContext.externalHost
	return parsed.String()
}

func sameGatewayProxyAuthority(leftHost, leftScheme, rightHost, rightScheme string) bool {
	left, ok := canonicalGatewayProxyAuthority(leftHost, firstNonEmpty(leftScheme, rightScheme))
	if !ok {
		return false
	}
	right, ok := canonicalGatewayProxyAuthority(rightHost, rightScheme)
	return ok && left == right
}

func canonicalGatewayProxyAuthority(hostValue, scheme string) (string, bool) {
	hostValue = strings.TrimSpace(hostValue)
	if hostValue == "" || strings.HasSuffix(hostValue, ".") {
		return "", false
	}
	host, port, err := net.SplitHostPort(hostValue)
	if err != nil {
		host = hostValue
		if strings.HasSuffix(host, ".") {
			return "", false
		}
		if strings.HasPrefix(host, "[") {
			end := strings.LastIndex(host, "]")
			if end < 0 {
				return "", false
			}
			host = host[1:end]
			if len(hostValue) > end+1 && strings.HasPrefix(hostValue[end+1:], ":") {
				port = strings.TrimPrefix(hostValue[end+1:], ":")
			}
		}
	}
	if strings.HasSuffix(host, ".") {
		return "", false
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return "", false
	}
	if port == "" {
		port = defaultGatewayProxyPort(scheme)
	}
	if port == "" {
		return host, true
	}
	return net.JoinHostPort(host, port), true
}

func defaultGatewayProxyPort(scheme string) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
