// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

func newGatewayHandlerWithOriginAndBrowserAuth(
	health *processHealth,
	sessions *gatewaySessionRegistry,
	access *gatewayAccessController,
	controlPlane gatewayControlPlaneClient,
	originPolicy gatewayOriginPolicy,
	browserAuth *gatewayBrowserAuth,
	routes ...*gatewayRouteStore,
) http.Handler {
	var routeStore *gatewayRouteStore
	if len(routes) > 0 && routes[0] != nil {
		routeStore = routes[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if originPolicy.domain == "" {
			writeGatewayNotFound(writer, request, "Codespace gateway")
			return
		}
		handleGatewayWorkspace(writer, request, sessions, routeStore, access, controlPlane, originPolicy, browserAuth)
	})
	mux.HandleFunc("/api/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		health.writeHealthz(writer)
	})
	mux.HandleFunc("/open", func(writer http.ResponseWriter, request *http.Request) {
		handleGatewayOpen(writer, request, sessions, access, controlPlane, originPolicy)
	})
	mux.HandleFunc("/.gitea-codespace/open", func(writer http.ResponseWriter, request *http.Request) {
		handleGatewayOpen(writer, request, sessions, access, controlPlane, originPolicy)
	})
	mux.HandleFunc("/w/", func(writer http.ResponseWriter, request *http.Request) {
		handleGatewayWorkspace(writer, request, sessions, routeStore, access, controlPlane, originPolicy, browserAuth)
	})
	mux.HandleFunc("/p/", func(writer http.ResponseWriter, request *http.Request) {
		handleGatewayPublicEndpoint(writer, request, routeStore, access, controlPlane, originPolicy)
	})
	return loggingMiddleware(mux)
}

func newGatewayHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: gatewayHTTPReadHeaderTime,
		MaxHeaderBytes:    gatewayHTTPMaxHeaderBytes,
	}
}

func handleGatewayOpen(
	writer http.ResponseWriter,
	request *http.Request,
	sessions *gatewaySessionRegistry,
	access *gatewayAccessController,
	controlPlane gatewayControlPlaneClient,
	originPolicy gatewayOriginPolicy,
) {
	setGatewayOpenResponseHeaders(writer)
	if rejectGatewayServiceWorkerRequest(writer, request) {
		return
	}
	if request.Method != http.MethodGet {
		writeGatewayError(writer, request, http.StatusMethodNotAllowed, "Method not allowed", "This gateway endpoint only accepts GET requests.", "method_not_allowed")
		return
	}
	if originPolicy.domain != "" && request.URL.Path != "/.gitea-codespace/open" {
		writeGatewayNotFound(writer, request, "Open codespace")
		return
	}
	if sessions == nil || access == nil || controlPlane == nil {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Gateway is starting", "Codespace Gateway is not ready yet. Try again after the manager finishes startup.", "gateway is not ready")
		return
	}
	hostBinding, hasHostBinding := originPolicy.bindingForRequest(request)
	if originPolicy.domain != "" && !hasHostBinding {
		writeGatewayNotFound(writer, request, "Open codespace")
		return
	}
	code, ok := gatewayOpenCode(request)
	if !ok {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusForbidden, "Open link is invalid", "The open link is missing a valid one-time code. Open the codespace again from Gitea.", "invalid open code request")
		return
	}
	reservation, limitStatus := access.reserveRequest()
	if limitStatus != 0 {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Gateway is busy", "Codespace Gateway has no request capacity available right now. Try again shortly.", "gateway capacity unavailable")
		return
	}
	defer reservation.Release()

	decision, err := controlPlane.validateOpenToken(request.Context(), code)
	if err != nil {
		log.Printf("validate open token: %v", err)
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is unavailable", "Codespace Gateway cannot confirm this open link with Gitea right now. Try again shortly.", "gateway authorization unavailable")
		return
	}
	if !decision.allowed {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusForbidden, "Codespace cannot be opened", "Gitea rejected this open link because the codespace is not currently available for this request.", decision.deniedCategory)
		return
	}
	if hasHostBinding &&
		(hostBinding.codespaceUUID != decision.binding.codespaceUUID ||
			hostBinding.endpointID != decision.binding.endpointID) {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusForbidden, "Open link does not match this host", "This open link belongs to a different codespace endpoint. Open the codespace again from Gitea.", "gateway host binding mismatch")
		return
	}
	replaceSessionIDs := gatewaySessionIDsFromRequest(request, originPolicy)
	sessionID, err := sessions.CreateReplacingAny(decision.binding, replaceSessionIDs, time.Now())
	if err != nil {
		log.Printf("create gateway session: %v", err)
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		if errors.Is(err, errGatewaySessionAmbiguous) {
			writeGatewayError(writer, request, http.StatusUnauthorized, "Session is ambiguous", "More than one gateway session matched this request. Open the codespace again from Gitea.", "gateway session is ambiguous")
			return
		}
		if errors.Is(err, errGatewaySessionLimitReached) {
			writeGatewayError(writer, request, http.StatusTooManyRequests, "Session limit reached", "This codespace or user already has the maximum number of gateway sessions.", "gateway session limit reached")
			return
		}
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Session is unavailable", "Codespace Gateway could not create a session for this open link. Try again shortly.", "gateway session unavailable")
		return
	}
	setGatewaySessionCookie(writer, sessionID, originPolicy)
	returnTo, hasReturnTo := gatewayReturnToPathFromRequest(request, originPolicy)
	if hasReturnTo {
		clearGatewayReturnToCookies(writer)
	}
	http.Redirect(writer, request, gatewayOpenRedirectPath(decision.binding.codespaceUUID, decision.binding.endpointID, originPolicy, returnTo), http.StatusSeeOther)
}

func setGatewayOpenResponseHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
}

func gatewayOpenCode(request *http.Request) (string, bool) {
	query := request.URL.Query()
	codes := query["code"]
	if len(query) != 1 || len(codes) != 1 || strings.TrimSpace(codes[0]) == "" {
		return "", false
	}
	return codes[0], true
}

func handleGatewayWorkspace(
	writer http.ResponseWriter,
	request *http.Request,
	sessions *gatewaySessionRegistry,
	routes *gatewayRouteStore,
	access *gatewayAccessController,
	controlPlane gatewayControlPlaneClient,
	originPolicy gatewayOriginPolicy,
	browserAuth *gatewayBrowserAuth,
) {
	if sessions == nil || access == nil || controlPlane == nil {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Gateway is starting", "Codespace Gateway is not ready yet. Try again after the manager finishes startup.", "gateway is not ready")
		return
	}
	codespaceUUID, endpointID, upstreamPath, ok := resolveGatewayWorkspaceBinding(request, originPolicy)
	if !ok {
		writeGatewayNotFound(writer, request, "Codespace workspace")
		return
	}
	if rejectGatewayServiceWorkerRequest(writer, request) {
		return
	}
	if !isGatewayAuthenticatedSourceAllowed(request, originPolicy) {
		writeGatewayError(writer, request, http.StatusForbidden, "Request source is not allowed", "This request did not come from an allowed codespace gateway origin.", "gateway source is not allowed")
		return
	}
	sessionIDs := gatewaySessionIDsFromRequest(request, originPolicy)
	if len(sessionIDs) == 0 {
		if handleGatewayAuthenticationRequired(writer, request, codespaceUUID, endpointID, originPolicy, browserAuth) {
			return
		}
		writeGatewayError(writer, request, http.StatusUnauthorized, "Sign in required", "Open this codespace from Gitea to create a gateway session.", "gateway session is required")
		return
	}
	session, ok, ambiguous := sessions.AuthenticateAny(sessionIDs, codespaceUUID, endpointID, time.Now())
	if ambiguous {
		writeGatewayError(writer, request, http.StatusUnauthorized, "Session is ambiguous", "More than one gateway session matched this request. Open the codespace again from Gitea.", "gateway session is ambiguous")
		return
	}
	if !ok {
		if handleGatewayAuthenticationRequired(writer, request, codespaceUUID, endpointID, originPolicy, browserAuth) {
			return
		}
		writeGatewayError(writer, request, http.StatusUnauthorized, "Session expired", "This gateway session is no longer valid. Open the codespace again from Gitea.", "gateway session is invalid")
		return
	}
	reservation, limitStatus := access.reserveSessionRequest(session.id)
	if limitStatus != 0 {
		if limitStatus == http.StatusTooManyRequests {
			writeGatewayError(writer, request, http.StatusTooManyRequests, "Too many requests", "This gateway session has too many concurrent requests. Close unused tabs and try again.", "gateway session request limit reached")
			return
		}
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Gateway is busy", "Codespace Gateway has no request capacity available right now. Try again shortly.", "gateway capacity unavailable")
		return
	}
	defer reservation.Release()

	decision, validationFull, err := access.validateEndpointSession(
		request.Context(),
		session.userID,
		session.codespaceUUID,
		session.endpointID,
		time.Now(),
		func(ctx context.Context) (gatewayAccessDecision, error) {
			return controlPlane.revalidateEndpointSession(ctx, session.userID, session.codespaceUUID, session.endpointID)
		},
	)
	if validationFull {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is busy", "Codespace Gateway has no authorization capacity available right now. Try again shortly.", "gateway authorization capacity unavailable")
		return
	}
	if err != nil {
		log.Printf("revalidate gateway session: %v", err)
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is unavailable", "Codespace Gateway cannot confirm this session with Gitea right now. Try again shortly.", "gateway authorization unavailable")
		return
	}
	if !decision.allowed {
		writeGatewayError(writer, request, http.StatusForbidden, "Codespace is unavailable", "Gitea reports that this codespace endpoint is not currently available for this session.", decision.deniedCategory)
		return
	}
	requestContext, cancelRequest := context.WithCancel(request.Context())
	defer cancelRequest()
	request = request.WithContext(requestContext)
	end := sessions.BeginSessionCancelable(session.id, session.codespaceUUID, cancelRequest)
	defer end()

	if routes == nil {
		writeJSON(writer, http.StatusOK, map[string]any{
			"codespace_uuid": session.codespaceUUID,
			"endpoint_id":    session.endpointID,
			"status":         "authorized",
		})
		return
	}
	revalidate := func(ctx context.Context) (gatewayAccessDecision, error) {
		decision, validationFull, err := access.revalidateEndpointSession(
			ctx,
			session.userID,
			session.codespaceUUID,
			session.endpointID,
			func(ctx context.Context) (gatewayAccessDecision, error) {
				return controlPlane.revalidateEndpointSession(ctx, session.userID, session.codespaceUUID, session.endpointID)
			},
		)
		if validationFull {
			return gatewayAccessDecision{}, errGatewayAccessLimitReached
		}
		return decision, err
	}
	proxyContext := gatewayProxyRequestContext{
		codespaceUUID:  session.codespaceUUID,
		endpointID:     session.endpointID,
		access:         "authenticated",
		userID:         session.userID,
		externalScheme: gatewayExternalScheme(request, originPolicy),
		externalHost:   request.Host,
	}
	route, routeRequest, releaseRoute, ok := routes.BeginProxy(request, session.codespaceUUID, session.endpointID)
	if !ok {
		title := "Endpoint is not ready"
		message := "The runtime endpoint is not ready yet. Try again after the service starts."
		if session.endpointID == runtimeendpoint.WorkspaceEndpointID {
			title = "Workspace is not ready"
			message = "The Web IDE is not ready yet. Try again after the codespace finishes starting."
		}
		writeGatewayError(writer, request, http.StatusServiceUnavailable, title, message, "gateway route unavailable")
		return
	}
	defer releaseRoute()
	proxyRequest, cancelRevalidation := withGatewayProxyRevalidation(routeRequest, access.config.streamRevalidateInterval, "revalidate gateway endpoint session", revalidate)
	defer cancelRevalidation()
	proxyGatewayEndpoint(writer, proxyRequest, routes, route, upstreamPath, proxyContext)
}

func handleGatewayPublicEndpoint(
	writer http.ResponseWriter,
	request *http.Request,
	routes *gatewayRouteStore,
	access *gatewayAccessController,
	controlPlane gatewayControlPlaneClient,
	originPolicy gatewayOriginPolicy,
) {
	if access == nil || controlPlane == nil {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Gateway is starting", "Codespace Gateway is not ready yet. Try again after the manager finishes startup.", "gateway is not ready")
		return
	}
	codespaceUUID, endpointID, upstreamPath, ok := resolveGatewayPublicEndpointBinding(request, originPolicy)
	if !ok {
		writeGatewayNotFound(writer, request, "Codespace endpoint")
		return
	}
	if rejectGatewayServiceWorkerRequest(writer, request) {
		return
	}
	clearGatewayReservedCookies(writer)
	if routes != nil {
		route, ok := routes.Get(codespaceUUID, endpointID)
		if !ok || !route.public {
			writeGatewayNotFound(writer, request, "Codespace endpoint")
			return
		}
	}
	reservation, limitStatus := access.reservePublic(codespaceUUID, endpointID, gatewayPeerIP(request))
	if limitStatus != 0 {
		if limitStatus == http.StatusTooManyRequests {
			writeGatewayError(writer, request, http.StatusTooManyRequests, "Connection limit reached", "This public endpoint has too many active connections. Try again shortly.", "gateway public connection limit reached")
			return
		}
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Gateway is busy", "Codespace Gateway has no request capacity available right now. Try again shortly.", "gateway capacity unavailable")
		return
	}
	defer reservation.Release()

	decision, validationFull, err := access.validatePublicEndpoint(
		request.Context(),
		codespaceUUID,
		endpointID,
		time.Now(),
		func(ctx context.Context) (gatewayAccessDecision, error) {
			return controlPlane.validatePublicEndpoint(ctx, codespaceUUID, endpointID)
		},
	)
	if validationFull {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is busy", "Codespace Gateway has no authorization capacity available right now. Try again shortly.", "gateway authorization capacity unavailable")
		return
	}
	if err != nil {
		log.Printf("validate public endpoint: %v", err)
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is unavailable", "Codespace Gateway cannot confirm this public endpoint with Gitea right now. Try again shortly.", "gateway authorization unavailable")
		return
	}
	if !decision.allowed {
		writeGatewayNotFound(writer, request, "Codespace endpoint")
		return
	}
	if routes == nil {
		writeJSON(writer, http.StatusOK, map[string]any{
			"access":         "public",
			"codespace_uuid": codespaceUUID,
			"endpoint_id":    endpointID,
			"status":         "authorized",
		})
		return
	}
	route, routeRequest, releaseRoute, ok := routes.BeginProxy(request, codespaceUUID, endpointID)
	if !ok || !route.public {
		if ok {
			releaseRoute()
		}
		writeGatewayNotFound(writer, request, "Codespace endpoint")
		return
	}
	defer releaseRoute()
	proxyRequest, cancelProxyRevalidation := withGatewayProxyRevalidation(
		routeRequest,
		access.config.streamRevalidateInterval,
		"revalidate public gateway endpoint",
		func(ctx context.Context) (gatewayAccessDecision, error) {
			decision, validationFull, err := access.revalidatePublicEndpoint(
				ctx,
				codespaceUUID,
				endpointID,
				func(ctx context.Context) (gatewayAccessDecision, error) {
					return controlPlane.validatePublicEndpoint(ctx, codespaceUUID, endpointID)
				},
			)
			if validationFull {
				return gatewayAccessDecision{}, errGatewayAccessLimitReached
			}
			return decision, err
		},
	)
	defer cancelProxyRevalidation()
	proxyGatewayEndpoint(writer, proxyRequest, routes, route, upstreamPath, gatewayProxyRequestContext{
		codespaceUUID:  codespaceUUID,
		endpointID:     endpointID,
		access:         "public",
		externalScheme: gatewayExternalScheme(request, originPolicy),
		externalHost:   request.Host,
	})
}

func parseGatewayWorkspacePath(path string) (string, string, string, bool) {
	withoutPrefix, ok := strings.CutPrefix(path, "/w/")
	if !ok {
		return "", "", "", false
	}
	trimmed := strings.Trim(withoutPrefix, "/")
	if trimmed == "" {
		return "", "", "", false
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 1 {
		return parts[0], runtimeendpoint.WorkspaceEndpointID, "/", true
	}
	if len(parts) >= 3 && parts[1] == "e" && parts[2] != "" && parts[2] != runtimeendpoint.WorkspaceEndpointID {
		return parts[0], parts[2], gatewayProxyPathFromParts(parts[3:]), true
	}
	return parts[0], runtimeendpoint.WorkspaceEndpointID, gatewayProxyPathFromParts(parts[1:]), true
}

func resolveGatewayWorkspaceBinding(request *http.Request, originPolicy gatewayOriginPolicy) (string, string, string, bool) {
	if originPolicy.domain == "" {
		return parseGatewayWorkspacePath(request.URL.Path)
	}
	hostBinding, ok := originPolicy.bindingForRequest(request)
	if !ok {
		return "", "", "", false
	}
	pathUUID, pathEndpoint, upstreamPath, pathOK := parseGatewayWorkspacePath(request.URL.Path)
	if pathOK && (pathUUID != hostBinding.codespaceUUID || pathEndpoint != hostBinding.endpointID) {
		return "", "", "", false
	}
	if !pathOK {
		if request.URL.Path == "/w/" {
			return hostBinding.codespaceUUID, hostBinding.endpointID, "/", true
		}
		return hostBinding.codespaceUUID, hostBinding.endpointID, request.URL.Path, true
	}
	return hostBinding.codespaceUUID, hostBinding.endpointID, upstreamPath, true
}

func parseGatewayPublicEndpointPath(path string) (string, string, string, bool) {
	withoutPrefix, ok := strings.CutPrefix(path, "/p/")
	if !ok {
		return "", "", "", false
	}
	trimmed := strings.Trim(withoutPrefix, "/")
	if trimmed == "" {
		return "", "", "", false
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" || parts[1] == runtimeendpoint.WorkspaceEndpointID {
		return "", "", "", false
	}
	return parts[0], parts[1], gatewayProxyPathFromParts(parts[2:]), true
}

func resolveGatewayPublicEndpointBinding(request *http.Request, originPolicy gatewayOriginPolicy) (string, string, string, bool) {
	if originPolicy.domain == "" {
		return parseGatewayPublicEndpointPath(request.URL.Path)
	}
	hostBinding, ok := originPolicy.bindingForRequest(request)
	if !ok || hostBinding.endpointID == runtimeendpoint.WorkspaceEndpointID {
		return "", "", "", false
	}
	pathUUID, pathEndpoint, upstreamPath, pathOK := parseGatewayPublicEndpointPath(request.URL.Path)
	if pathOK && (pathUUID != hostBinding.codespaceUUID || pathEndpoint != hostBinding.endpointID) {
		return "", "", "", false
	}
	if !pathOK && request.URL.Path != "/p/" {
		return "", "", "", false
	}
	if !pathOK {
		upstreamPath = "/"
	}
	return hostBinding.codespaceUUID, hostBinding.endpointID, upstreamPath, true
}

func gatewayProxyPathFromParts(parts []string) string {
	if len(parts) == 0 {
		return "/"
	}
	return "/" + strings.Join(parts, "/")
}

func withGatewayProxyRevalidation(
	request *http.Request,
	interval time.Duration,
	logMessage string,
	validate func(context.Context) (gatewayAccessDecision, error),
) (*http.Request, context.CancelFunc) {
	if interval <= 0 {
		interval = defaultGatewaySessionRevalidateInterval
	}
	ctx, cancel := context.WithCancel(request.Context())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				decision, err := validate(ctx)
				if err != nil {
					log.Printf("%s: %v", logMessage, err)
					cancel()
					return
				}
				if !decision.allowed {
					cancel()
					return
				}
			}
		}
	}()
	return request.WithContext(ctx), cancel
}

func gatewayExternalScheme(request *http.Request, originPolicy gatewayOriginPolicy) string {
	if originPolicy.scheme != "" {
		return originPolicy.scheme
	}
	if request.TLS != nil {
		return "https"
	}
	return "http"
}

func gatewayPeerIP(request *http.Request) string {
	if request == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	if parsed := net.ParseIP(request.RemoteAddr); parsed != nil {
		return parsed.String()
	}
	return request.RemoteAddr
}

func rejectGatewayServiceWorkerRequest(writer http.ResponseWriter, request *http.Request) bool {
	if !isGatewayServiceWorkerRequest(request) {
		return false
	}
	writer.Header().Del("Service-Worker-Allowed")
	writeGatewayError(writer, request, http.StatusForbidden, "Service worker is not allowed", "Codespace Gateway does not allow runtime pages to register a service worker on the gateway origin.", "service worker is not allowed")
	return true
}

func isGatewayServiceWorkerRequest(request *http.Request) bool {
	if request == nil {
		return false
	}
	if values := request.Header.Values("Service-Worker"); len(values) > 0 {
		return true
	}
	values := request.Header.Values("Sec-Fetch-Dest")
	if len(values) == 0 {
		return false
	}
	if len(values) > 1 {
		return true
	}
	value := strings.TrimSpace(values[0])
	return value == "" || strings.EqualFold(value, "serviceworker")
}

func gatewayWorkspacePath(codespaceUUID, endpointID string) string {
	if endpointID == "" || endpointID == runtimeendpoint.WorkspaceEndpointID {
		return "/w/" + codespaceUUID + "/"
	}
	return "/w/" + codespaceUUID + "/e/" + endpointID + "/"
}

func gatewayOpenRedirectPath(codespaceUUID, endpointID string, originPolicy gatewayOriginPolicy, returnTo string) string {
	if returnTo != "" {
		return returnTo
	}
	if originPolicy.domain != "" {
		return "/"
	}
	return gatewayWorkspacePath(codespaceUUID, endpointID)
}

func setGatewaySessionCookie(writer http.ResponseWriter, sessionID string, originPolicy gatewayOriginPolicy) {
	cookie := &http.Cookie{
		Name:     gatewaySessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if strings.EqualFold(originPolicy.scheme, "https") {
		cookie.Name = gatewaySecureSessionCookieName
		cookie.Secure = true
	}
	http.SetCookie(writer, cookie)
}

func gatewaySessionIDsFromRequest(request *http.Request, originPolicy gatewayOriginPolicy) []string {
	name := gatewaySessionCookieName
	if strings.EqualFold(originPolicy.scheme, "https") {
		name = gatewaySecureSessionCookieName
	}
	values := make(map[string]struct{})
	for _, cookie := range parseGatewayProxyRequestCookies(request.Header.Values("Cookie")) {
		if cookie.Name == name && cookie.Value != "" {
			values[cookie.Value] = struct{}{}
		}
	}
	ids := make([]string, 0, len(values))
	for value := range values {
		ids = append(ids, value)
	}
	return ids
}

func clearGatewayReservedCookies(writer http.ResponseWriter) {
	clearGatewaySessionCookies(writer)
	clearGatewayReturnToCookies(writer)
}

func clearGatewaySessionCookies(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name:     gatewaySessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(writer, &http.Cookie{
		Name:     gatewaySecureSessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func writeJSON(writer http.ResponseWriter, statusCode int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		log.Printf("encode json response: %v", err)
	}
}

func writeGatewayNotFound(writer http.ResponseWriter, request *http.Request, context string) {
	writeGatewayError(writer, request, http.StatusNotFound, "Page not found", context+" was not found on this gateway.", "not_found")
}

func writeGatewayError(writer http.ResponseWriter, request *http.Request, statusCode int, title, message, category string) {
	if gatewayRequestAcceptsHTML(request) {
		writeGatewayErrorHTML(writer, statusCode, title, message, category)
		return
	}
	writeJSON(writer, statusCode, map[string]any{"error": category})
}

func gatewayRequestAcceptsHTML(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") {
		return false
	}
	accept := request.Header.Get("Accept")
	if accept == "" || strings.Contains(accept, "application/json") {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(mediaType, "text/html") {
			return true
		}
	}
	return false
}

//go:embed templates/gateway_error.tmpl
var gatewayErrorTemplateSource string

var gatewayErrorTemplate = template.Must(template.New("gateway-error").Parse(gatewayErrorTemplateSource))

func writeGatewayErrorHTML(writer http.ResponseWriter, statusCode int, title, message, category string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	writer.WriteHeader(statusCode)
	if err := gatewayErrorTemplate.Execute(writer, struct {
		Status                               int
		StatusText, Title, Message, Category string
	}{statusCode, http.StatusText(statusCode), title, message, category}); err != nil {
		log.Printf("render gateway error page: %v", err)
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		log.Printf("%s %s", request.Method, request.URL.Path)
		next.ServeHTTP(writer, request)
	})
}
