// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitea.dev/codespace/internal/manager"
)

const (
	gatewayReturnToMaxBytes   = 2048
	gatewayReturnToCookieLife = 5 * time.Minute
)

type gatewayBrowserAuth struct {
	mu          sync.RWMutex
	giteaWebURL string
}

func newGatewayBrowserAuth() *gatewayBrowserAuth {
	return &gatewayBrowserAuth{}
}

func (a *gatewayBrowserAuth) SaveManagerServiceSettings(settings manager.ManagerServiceSettings) error {
	if _, err := parseGatewayGiteaWebURL(settings.GiteaWebURL); err != nil {
		return err
	}
	a.mu.Lock()
	a.giteaWebURL = settings.GiteaWebURL
	a.mu.Unlock()
	return nil
}

func (a *gatewayBrowserAuth) openURL(codespaceUUID, endpointID string) (string, bool) {
	if a == nil {
		return "", false
	}
	a.mu.RLock()
	rawURL := a.giteaWebURL
	a.mu.RUnlock()
	parsed, err := parseGatewayGiteaWebURL(rawURL)
	if err != nil {
		return "", false
	}
	openPath := strings.TrimRight(parsed.Path, "/") + "/-/codespaces/" + url.PathEscape(codespaceUUID) + "/open"
	if endpointID != "" && endpointID != "workspace" {
		openPath += "/" + url.PathEscape(endpointID)
	}
	parsed.Path = openPath
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), true
}

func parseGatewayGiteaWebURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse gitea web url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("gitea web url must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("gitea web url must include only scheme, host, and path")
	}
	if parsed.Path == "" || !strings.HasSuffix(parsed.Path, "/") {
		return nil, fmt.Errorf("gitea web url path must end with slash")
	}
	return parsed, nil
}

func handleGatewayAuthenticationRequired(
	writer http.ResponseWriter,
	request *http.Request,
	codespaceUUID string,
	endpointID string,
	originPolicy gatewayOriginPolicy,
	browserAuth *gatewayBrowserAuth,
) bool {
	if !isGatewayBrowserAuthNavigation(request) {
		return false
	}
	openURL, ok := browserAuth.openURL(codespaceUUID, endpointID)
	if !ok {
		return false
	}
	setGatewayReturnToCookie(writer, gatewayReturnToCookie(request), originPolicy)
	http.Redirect(writer, request, openURL, http.StatusSeeOther)
	return true
}

func isGatewayBrowserAuthNavigation(request *http.Request) bool {
	if request.Method != http.MethodGet {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") {
		return false
	}
	if !headerAcceptsHTML(request.Header.Get("Accept")) {
		return false
	}
	if mode := request.Header.Get("Sec-Fetch-Mode"); !strings.EqualFold(strings.TrimSpace(mode), "navigate") {
		return false
	}
	if dest := request.Header.Get("Sec-Fetch-Dest"); !strings.EqualFold(strings.TrimSpace(dest), "document") {
		return false
	}
	return true
}

func headerAcceptsHTML(value string) bool {
	for _, part := range strings.Split(value, ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(mediaType, "text/html") || mediaType == "*/*" {
			return true
		}
	}
	return false
}

func gatewayReturnToCookie(request *http.Request) string {
	value := request.URL.EscapedPath()
	if value == "" {
		value = "/"
	}
	if request.URL.RawQuery != "" {
		value += "?" + request.URL.RawQuery
	}
	return sanitizeGatewayReturnTo(value)
}

func sanitizeGatewayReturnTo(value string) string {
	if len(value) > gatewayReturnToMaxBytes {
		return "/"
	}
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return "/"
	}
	if strings.Contains(value, `\`) || strings.Contains(value, "/.gitea-codespace/") {
		return "/"
	}
	if strings.ContainsFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) {
		return "/"
	}
	return value
}

func setGatewayReturnToCookie(writer http.ResponseWriter, value string, originPolicy gatewayOriginPolicy) {
	scheme := strings.ToLower(originPolicy.scheme)
	cookie := &http.Cookie{
		Name:     gatewayReturnToCookieName,
		Value:    sanitizeGatewayReturnTo(value),
		Path:     "/",
		MaxAge:   int(gatewayReturnToCookieLife / time.Second),
		Expires:  time.Now().Add(gatewayReturnToCookieLife),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if scheme == "https" {
		cookie.Name = gatewaySecureReturnToCookieName
		cookie.Secure = true
	}
	http.SetCookie(writer, cookie)
}

func gatewayReturnToPathFromRequest(request *http.Request, originPolicy gatewayOriginPolicy) (string, bool) {
	name := gatewayReturnToCookieName
	if strings.EqualFold(originPolicy.scheme, "https") {
		name = gatewaySecureReturnToCookieName
	}
	var values []string
	hasReturnToCookie := false
	for _, cookie := range request.Cookies() {
		if cookie.Name == gatewayReturnToCookieName || cookie.Name == gatewaySecureReturnToCookieName {
			hasReturnToCookie = true
		}
		if cookie.Name == name {
			values = append(values, cookie.Value)
		}
	}
	if len(values) == 0 {
		return "", hasReturnToCookie
	}
	if len(values) != 1 {
		return "/", true
	}
	return sanitizeGatewayReturnTo(values[0]), true
}

func clearGatewayReturnToIfPresent(writer http.ResponseWriter, request *http.Request, originPolicy gatewayOriginPolicy) {
	if _, ok := gatewayReturnToPathFromRequest(request, originPolicy); ok {
		clearGatewayReturnToCookies(writer)
	}
}

func clearGatewayReturnToCookies(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name:     gatewayReturnToCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(writer, &http.Cookie{
		Name:     gatewaySecureReturnToCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
