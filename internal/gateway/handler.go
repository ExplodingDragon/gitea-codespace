// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

func newHandler(
	sessions *sessionRegistry,
	access *accessController,
	controlPlane gatewayControlPlaneClient,
	originPolicy originPolicy,
	browserAuth *browserAuth,
	routeStore *routeStore,
) http.Handler {
	if routeStore == nil {
		panic("gateway route store is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if originPolicy.Domain == "" {
			writeGatewayNotFound(writer, request, "Codespace gateway")
			return
		}
		if binding, ok := originPolicy.bindingForRequest(request); ok {
			if route, found := routeStore.Get(binding.codespaceUUID, binding.endpointID); found && route.Public {
				handleGatewayPublicEndpoint(writer, request, routeStore, access, controlPlane, originPolicy)
				return
			}
		}
		handleGatewayWorkspace(writer, request, sessions, routeStore, access, controlPlane, originPolicy, browserAuth)
	})
	mux.HandleFunc("/open", func(writer http.ResponseWriter, request *http.Request) {
		handleGatewayOpen(writer, request, sessions, routeStore, access, controlPlane, originPolicy)
	})
	mux.HandleFunc("/.gitea-codespace/open", func(writer http.ResponseWriter, request *http.Request) {
		handleGatewayOpen(writer, request, sessions, routeStore, access, controlPlane, originPolicy)
	})
	mux.HandleFunc("/w/", func(writer http.ResponseWriter, request *http.Request) {
		handleGatewayWorkspace(writer, request, sessions, routeStore, access, controlPlane, originPolicy, browserAuth)
	})
	mux.HandleFunc("/p/", func(writer http.ResponseWriter, request *http.Request) {
		handleGatewayPublicEndpoint(writer, request, routeStore, access, controlPlane, originPolicy)
	})
	return mux
}

func handleGatewayOpen(
	writer http.ResponseWriter,
	request *http.Request,
	sessions *sessionRegistry,
	routes *routeStore,
	access *accessController,
	controlPlane gatewayControlPlaneClient,
	originPolicy originPolicy,
) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	if request.Method != http.MethodGet {
		writeGatewayError(writer, request, http.StatusMethodNotAllowed, "Method not allowed", "This gateway endpoint only accepts GET requests.", "method_not_allowed")
		return
	}
	if originPolicy.Domain != "" && request.URL.Path != "/.gitea-codespace/open" {
		writeGatewayNotFound(writer, request, "Open codespace")
		return
	}
	if sessions == nil || access == nil || controlPlane == nil {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Gateway is starting", "Codespace Gateway is not ready yet. Try again after the manager finishes startup.", "gateway is not ready")
		return
	}
	hostBinding, hasHostBinding := originPolicy.bindingForRequest(request)
	if originPolicy.Domain != "" && !hasHostBinding {
		writeGatewayNotFound(writer, request, "Open codespace")
		return
	}
	code, ok := gatewayOpenCode(request)
	if !ok {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusForbidden, "Open link is invalid", "The open link is missing a valid one-time code. Open the codespace again from Gitea.", "invalid open code request")
		return
	}
	reservation, limitStatus := access.ReserveRequest()
	if limitStatus != 0 {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Gateway is busy", "Codespace Gateway has no request capacity available right now. Try again shortly.", "gateway capacity unavailable")
		return
	}
	defer reservation.Release()

	decision, err := controlPlane.ValidateOpenToken(request.Context(), code, hostBinding.codespaceUUID, hostBinding.endpointID)
	if err != nil {
		attrs := gatewayRequestLogAttrs(request)
		attrs = append(attrs,
			slog.String("stage", "authorization"),
			slog.Int("status_code", http.StatusServiceUnavailable),
			slog.String("codespace_uuid", hostBinding.codespaceUUID),
			slog.String("endpoint_id", hostBinding.endpointID),
			slog.Any("error", err),
		)
		gatewayLog(request.Context(), slog.LevelWarn, "gateway_authorization_failed", attrs...)
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is unavailable", "Codespace Gateway cannot confirm this open link with Gitea right now. Try again shortly.", "gateway authorization unavailable")
		return
	}
	if !decision.Allowed {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusForbidden, "Codespace cannot be opened", "Gitea rejected this open link because the codespace is not currently available for this request.", decision.deniedCategory)
		return
	}
	if hasHostBinding &&
		(hostBinding.codespaceUUID != decision.Binding.CodespaceUUID ||
			hostBinding.endpointID != decision.Binding.EndpointID) {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusForbidden, "Open link does not match this host", "This open link belongs to a different codespace endpoint. Open the codespace again from Gitea.", "gateway host binding mismatch")
		return
	}
	route, found := routes.Get(decision.Binding.CodespaceUUID, decision.Binding.EndpointID)
	if !found || route.SiteUID != decision.Binding.SiteUID {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeGatewayError(writer, request, http.StatusForbidden, "Open link does not match this runtime", "This open link belongs to a different codespace runtime. Open the codespace again from Gitea.", "gateway route site mismatch")
		return
	}
	replaceSessionIDs := gatewaySessionIDsFromRequest(request, originPolicy)
	sessionID, err := sessions.CreateReplacingAny(decision.Binding, replaceSessionIDs, time.Now())
	if err != nil {
		statusCode := http.StatusServiceUnavailable
		if errors.Is(err, errGatewaySessionAmbiguous) {
			statusCode = http.StatusUnauthorized
		} else if errors.Is(err, errGatewaySessionLimitReached) {
			statusCode = http.StatusTooManyRequests
		}
		attrs := gatewayRequestLogAttrs(request)
		attrs = append(attrs, gatewayRouteLogAttrs(route)...)
		attrs = append(attrs,
			slog.String("stage", "session_create"),
			slog.Int("status_code", statusCode),
			slog.Int64("user_id", decision.Binding.UserID),
			slog.Any("error", err),
		)
		gatewayLog(request.Context(), slog.LevelWarn, "gateway_session_create_failed", attrs...)
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
	http.Redirect(writer, request, gatewayOpenRedirectPath(decision.Binding.CodespaceUUID, decision.Binding.EndpointID, originPolicy, returnTo), http.StatusSeeOther)
}

func gatewayOpenCode(request *http.Request) (string, bool) {
	query := request.URL.Query()
	codes := query["code"]
	if len(codes) != 1 || strings.TrimSpace(codes[0]) == "" {
		return "", false
	}
	return codes[0], true
}

func handleGatewayWorkspace(
	writer http.ResponseWriter,
	request *http.Request,
	sessions *sessionRegistry,
	routes *routeStore,
	access *accessController,
	controlPlane gatewayControlPlaneClient,
	originPolicy originPolicy,
	browserAuth *browserAuth,
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
	if !isGatewayAuthenticatedSourceAllowed(request, originPolicy) {
		attrs := gatewaySourceRejectionLogAttrs(request, originPolicy)
		attrs = append(attrs,
			slog.String("codespace_uuid", codespaceUUID),
			slog.String("endpoint_id", endpointID),
			slog.String("access", "authenticated"),
			slog.String("reason", gatewaySourceRejectionReason(request, originPolicy)),
			slog.Int("status_code", http.StatusForbidden),
		)
		gatewayLog(request.Context(), slog.LevelWarn, "gateway_source_rejected", attrs...)
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
	route, found := routes.Get(session.codespaceUUID, session.endpointID)
	if !found {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Endpoint is not ready", "The runtime endpoint is not ready yet. Try again after the service starts.", "gateway route unavailable")
		return
	}
	if route.SiteUID != session.siteUID {
		writeGatewayError(writer, request, http.StatusUnauthorized, "Session is no longer valid", "This gateway session belongs to a different codespace runtime. Open the codespace again from Gitea.", "gateway session site mismatch")
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
		session.siteUID,
		session.userID,
		session.codespaceUUID,
		session.endpointID,
		time.Now(),
		func(ctx context.Context) (gatewayAccessDecision, error) {
			return controlPlane.RevalidateEndpointSession(ctx, session.siteUID, session.userID, session.codespaceUUID, session.endpointID)
		},
	)
	if validationFull {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is busy", "Codespace Gateway has no authorization capacity available right now. Try again shortly.", "gateway authorization capacity unavailable")
		return
	}
	if err != nil {
		attrs := gatewayRequestLogAttrs(request)
		attrs = append(attrs, gatewayRouteLogAttrs(route)...)
		attrs = append(attrs,
			slog.String("stage", "authorization"),
			slog.Int("status_code", http.StatusServiceUnavailable),
			slog.String("access", "authenticated"),
			slog.Int64("user_id", session.userID),
			slog.Any("error", err),
		)
		gatewayLog(request.Context(), slog.LevelWarn, "gateway_authorization_failed", attrs...)
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is unavailable", "Codespace Gateway cannot confirm this session with Gitea right now. Try again shortly.", "gateway authorization unavailable")
		return
	}
	if !decision.Allowed {
		writeGatewayError(writer, request, http.StatusForbidden, "Codespace is unavailable", "Gitea reports that this codespace endpoint is not currently available for this session.", decision.DeniedCategory)
		return
	}
	setGatewaySessionCookie(writer, session.token, originPolicy)
	requestContext, cancelRequest := context.WithCancel(request.Context())
	defer cancelRequest()
	request = request.WithContext(requestContext)
	end := sessions.BeginSessionCancelable(session.id, session.codespaceUUID, cancelRequest)
	defer end()

	revalidate := func(ctx context.Context) (gatewayAccessDecision, error) {
		decision, validationFull, err := access.RevalidateEndpointSession(
			ctx,
			session.siteUID,
			session.userID,
			session.codespaceUUID,
			session.endpointID,
			func(ctx context.Context) (gatewayAccessDecision, error) {
				return controlPlane.RevalidateEndpointSession(ctx, session.siteUID, session.userID, session.codespaceUUID, session.endpointID)
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
	if !ok || route.SiteUID != session.siteUID {
		if ok {
			releaseRoute()
		}
		title := "Endpoint is not ready"
		message := "The runtime endpoint is not ready yet. Try again after the service starts."
		if session.endpointID == runtimeendpoint.WorkspaceEndpointID {
			title = "Workspace is unavailable"
			message = "The Web IDE endpoint is unavailable. SSH and other Codespace access remain independent."
		}
		writeGatewayError(writer, request, http.StatusServiceUnavailable, title, message, "gateway route unavailable")
		return
	}
	defer releaseRoute()
	revalidationAttrs := gatewayProxyLogAttrs(request, route, upstreamPath, proxyContext)
	proxyRequest, cancelRevalidation := withGatewayProxyRevalidation(routeRequest, access.Config.StreamRevalidateInterval, revalidationAttrs, revalidate)
	defer cancelRevalidation()
	proxyGatewayEndpoint(writer, proxyRequest, routes, route, upstreamPath, proxyContext)
}

func handleGatewayPublicEndpoint(
	writer http.ResponseWriter,
	request *http.Request,
	routes *routeStore,
	access *accessController,
	controlPlane gatewayControlPlaneClient,
	originPolicy originPolicy,
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
	clearGatewayReservedCookies(writer)
	route, ok := routes.Get(codespaceUUID, endpointID)
	if !ok || !route.Public {
		writeGatewayNotFound(writer, request, "Codespace endpoint")
		return
	}
	siteUID := route.SiteUID
	reservation, limitStatus := access.reservePublic(siteUID, codespaceUUID, endpointID, gatewayPeerIP(request))
	if limitStatus != 0 {
		if limitStatus == http.StatusTooManyRequests {
			writeGatewayError(writer, request, http.StatusTooManyRequests, "Connection limit reached", "This public endpoint has too many active connections. Try again shortly.", "gateway public connection limit reached")
			return
		}
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Gateway is busy", "Codespace Gateway has no request capacity available right now. Try again shortly.", "gateway capacity unavailable")
		return
	}
	defer reservation.Release()

	decision, validationFull, err := access.ValidatePublicEndpoint(
		request.Context(),
		siteUID,
		codespaceUUID,
		endpointID,
		time.Now(),
		func(ctx context.Context) (gatewayAccessDecision, error) {
			return controlPlane.ValidatePublicEndpoint(ctx, siteUID, codespaceUUID, endpointID)
		},
	)
	if validationFull {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is busy", "Codespace Gateway has no authorization capacity available right now. Try again shortly.", "gateway authorization capacity unavailable")
		return
	}
	if err != nil {
		attrs := gatewayRequestLogAttrs(request)
		attrs = append(attrs, gatewayRouteLogAttrs(route)...)
		attrs = append(attrs,
			slog.String("stage", "authorization"),
			slog.Int("status_code", http.StatusServiceUnavailable),
			slog.String("access", "public"),
			slog.Any("error", err),
		)
		gatewayLog(request.Context(), slog.LevelWarn, "gateway_authorization_failed", attrs...)
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Authorization is unavailable", "Codespace Gateway cannot confirm this public endpoint with Gitea right now. Try again shortly.", "gateway authorization unavailable")
		return
	}
	if !decision.Allowed {
		writeGatewayNotFound(writer, request, "Codespace endpoint")
		return
	}
	route, routeRequest, releaseRoute, ok := routes.BeginProxy(request, codespaceUUID, endpointID)
	if !ok || !route.Public || route.SiteUID != siteUID {
		if ok {
			releaseRoute()
		}
		writeGatewayNotFound(writer, request, "Codespace endpoint")
		return
	}
	defer releaseRoute()
	proxyContext := gatewayProxyRequestContext{
		codespaceUUID: codespaceUUID, endpointID: endpointID, access: "public",
		externalScheme: gatewayExternalScheme(request, originPolicy), externalHost: request.Host,
	}
	proxyRequest, cancelProxyRevalidation := withGatewayProxyRevalidation(
		routeRequest,
		access.Config.StreamRevalidateInterval,
		gatewayProxyLogAttrs(request, route, upstreamPath, proxyContext),
		func(ctx context.Context) (gatewayAccessDecision, error) {
			decision, validationFull, err := access.revalidatePublicEndpoint(
				ctx,
				siteUID,
				codespaceUUID,
				endpointID,
				func(ctx context.Context) (gatewayAccessDecision, error) {
					return controlPlane.ValidatePublicEndpoint(ctx, siteUID, codespaceUUID, endpointID)
				},
			)
			if validationFull {
				return gatewayAccessDecision{}, errGatewayAccessLimitReached
			}
			return decision, err
		},
	)
	defer cancelProxyRevalidation()
	proxyGatewayEndpoint(writer, proxyRequest, routes, route, upstreamPath, proxyContext)
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

func resolveGatewayWorkspaceBinding(request *http.Request, originPolicy originPolicy) (string, string, string, bool) {
	if originPolicy.Domain == "" {
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

func resolveGatewayPublicEndpointBinding(request *http.Request, originPolicy originPolicy) (string, string, string, bool) {
	if originPolicy.Domain == "" {
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
	if !pathOK {
		upstreamPath = request.URL.Path
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
	logAttrs []slog.Attr,
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
					if ctx.Err() != nil {
						return
					}
					attrs := append([]slog.Attr{}, logAttrs...)
					attrs = append(attrs,
						slog.String("stage", "authorization"),
						slog.Int("status_code", http.StatusServiceUnavailable),
						slog.Any("error", err),
					)
					gatewayLog(ctx, slog.LevelWarn, "gateway_authorization_failed", attrs...)
					continue
				}
				if !decision.Allowed {
					attrs := append([]slog.Attr{}, logAttrs...)
					attrs = append(attrs,
						slog.String("stage", "authorization"),
						slog.String("reason", decision.DeniedCategory),
					)
					gatewayLog(ctx, slog.LevelInfo, "gateway_authorization_revoked", attrs...)
					cancel()
					return
				}
			}
		}
	}()
	return request.WithContext(ctx), cancel
}

func gatewayExternalScheme(request *http.Request, originPolicy originPolicy) string {
	if originPolicy.Scheme != "" {
		return originPolicy.Scheme
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
	if address, ok := request.Context().Value(gatewayClientIPContextKey{}).(string); ok {
		return address
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

func gatewayWorkspacePath(codespaceUUID, endpointID string) string {
	if endpointID == "" || endpointID == runtimeendpoint.WorkspaceEndpointID {
		return "/w/" + codespaceUUID + "/"
	}
	return "/w/" + codespaceUUID + "/e/" + endpointID + "/"
}

func gatewayOpenRedirectPath(codespaceUUID, endpointID string, originPolicy originPolicy, returnTo string) string {
	if returnTo != "" {
		return returnTo
	}
	if originPolicy.Domain != "" {
		return "/"
	}
	return gatewayWorkspacePath(codespaceUUID, endpointID)
}

func setGatewaySessionCookie(writer http.ResponseWriter, sessionID string, originPolicy originPolicy) {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if strings.EqualFold(originPolicy.Scheme, "https") {
		cookie.Name = secureSessionCookieName
		cookie.Secure = true
	}
	http.SetCookie(writer, cookie)
}

func gatewaySessionIDsFromRequest(request *http.Request, originPolicy originPolicy) []string {
	name := sessionCookieName
	if strings.EqualFold(originPolicy.Scheme, "https") {
		name = secureSessionCookieName
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
	http.SetCookie(writer, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(writer, &http.Cookie{
		Name:     secureSessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	clearGatewayReturnToCookies(writer)
}

func writeGatewayNotFound(writer http.ResponseWriter, request *http.Request, context string) {
	writeGatewayError(writer, request, http.StatusNotFound, "Page not found", context+" was not found on this gateway.", "not_found")
}

func writeGatewayError(writer http.ResponseWriter, request *http.Request, statusCode int, title, message, category string) {
	if gatewayRequestAcceptsHTML(request) {
		writeGatewayErrorHTML(writer, request, statusCode, title, message, category)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	if err := json.NewEncoder(writer).Encode(map[string]any{"error": category}); err != nil {
		attrs := gatewayRequestLogAttrs(request)
		attrs = append(attrs, slog.String("stage", "response_write"), slog.Int("status_code", statusCode), slog.Any("error", err))
		gatewayLog(request.Context(), slog.LevelError, "gateway_response_write_failed", attrs...)
	}
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

func writeGatewayErrorHTML(writer http.ResponseWriter, request *http.Request, statusCode int, title, message, category string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	writer.WriteHeader(statusCode)
	if err := gatewayErrorTemplate.Execute(writer, struct {
		Status                               int
		StatusText, Title, Message, Category string
	}{statusCode, http.StatusText(statusCode), title, message, category}); err != nil {
		attrs := gatewayRequestLogAttrs(request)
		attrs = append(attrs, slog.String("stage", "response_write"), slog.Int("status_code", statusCode), slog.Any("error", err))
		gatewayLog(request.Context(), slog.LevelError, "gateway_response_write_failed", attrs...)
	}
}
