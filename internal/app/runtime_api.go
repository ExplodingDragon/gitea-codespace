// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"

	"gitea.dev/codespace/internal/provisioner"
	"golang.org/x/crypto/ssh"
)

const (
	runtimeEndpointAPIPrefix = "/api/runtime/v1/endpoints/"
	runtimeGitSSHKeyAPIPath  = "/api/runtime/v1/git-ssh-key"
)

type runtimeAPIService struct {
	state          *CodespaceStateStore
	routes         *gatewayRouteStore
	controlPlane   *gatewayControlPlane
	sourceResolver runtimeSourceResolver
}

type runtimeSourceResolver interface {
	ResolveRuntimeSource(ctx context.Context, sourceIP string) (provisioner.RuntimeSource, bool, error)
}

type runtimeAuthentication struct {
	codespaceUUID  string
	peerIP         string
	ok             bool
	bindingMatched bool
}

type runtimeEndpointRequest struct {
	Label          string `json:"label"`
	UpstreamScheme string `json:"upstream_scheme"`
	UpstreamPort   int    `json:"upstream_port"`
	Public         *bool  `json:"public"`
}

type runtimeEndpointResponse struct {
	EndpointID     string `json:"endpoint_id"`
	Label          string `json:"label"`
	UpstreamScheme string `json:"upstream_scheme"`
	UpstreamPort   int    `json:"upstream_port"`
	Public         bool   `json:"public"`
}

type runtimeGitSSHKeyRequest struct {
	PublicKey string `json:"public_key"`
}

type runtimeGitSSHKeyResponse struct {
	KnownHostsLines []string `json:"known_hosts_lines"`
}

func newRuntimeAPIService(
	state *CodespaceStateStore,
	routes *gatewayRouteStore,
	controlPlane *gatewayControlPlane,
	sourceResolver runtimeSourceResolver,
) *runtimeAPIService {
	return &runtimeAPIService{
		state:          state,
		routes:         routes,
		controlPlane:   controlPlane,
		sourceResolver: sourceResolver,
	}
}

func (s *runtimeAPIService) handleEndpoint(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.state == nil || s.routes == nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	endpointID, ok := runtimeEndpointIDFromPath(request.URL.Path)
	if !ok {
		http.NotFound(writer, request)
		return
	}
	auth, err := s.authenticate(request)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if !auth.ok {
		writeRuntimeAPIError(writer, http.StatusUnauthorized, "invalid_runtime_token")
		return
	}
	if !auth.bindingMatched {
		writeRuntimeAPIError(writer, http.StatusForbidden, "runtime_binding_mismatch")
		return
	}
	operationType, err := s.state.RuntimeAPIOperation(auth.codespaceUUID)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if operationType == runtimeAPIOperationStop || operationType == runtimeAPIOperationDelete {
		writeRuntimeAPIError(writer, http.StatusConflict, "operation_conflict")
		return
	}

	switch request.Method {
	case http.MethodGet:
		s.handleGetEndpoint(writer, auth.codespaceUUID, endpointID)
	case http.MethodPost:
		s.handlePostEndpoint(writer, request, auth.codespaceUUID, endpointID, auth.peerIP)
	case http.MethodPut:
		s.handlePutEndpoint(writer, request, auth.codespaceUUID, endpointID, auth.peerIP)
	case http.MethodDelete:
		s.handleDeleteEndpoint(writer, request, auth.codespaceUUID, endpointID)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *runtimeAPIService) handleGitSSHKey(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.state == nil || s.controlPlane == nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if request.Method != http.MethodPut {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	auth, err := s.authenticate(request)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if !auth.ok {
		writeRuntimeAPIError(writer, http.StatusUnauthorized, "invalid_runtime_token")
		return
	}
	if !auth.bindingMatched {
		writeRuntimeAPIError(writer, http.StatusForbidden, "runtime_binding_mismatch")
		return
	}
	operationType, err := s.state.RuntimeAPIOperation(auth.codespaceUUID)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if operationType != runtimeAPIOperationCreate && operationType != runtimeAPIOperationResume {
		writeRuntimeAPIError(writer, http.StatusConflict, "operation_conflict")
		return
	}

	var payload runtimeGitSSHKeyRequest
	if !decodeRuntimeJSON(writer, request, &payload) {
		return
	}
	publicKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(payload.PublicKey)))
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	lines, err := s.controlPlane.ensureCodespaceGitSSHKey(request.Context(), auth.codespaceUUID, publicKey.Marshal())
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, runtimeGitSSHKeyResponse{KnownHostsLines: lines})
}

func (s *runtimeAPIService) authenticate(request *http.Request) (runtimeAuthentication, error) {
	token, ok := runtimeBearerToken(request)
	if !ok {
		return runtimeAuthentication{}, nil
	}
	codespaceUUID, ok, err := s.state.ResolveRuntimeToken(token)
	if err != nil || !ok {
		return runtimeAuthentication{ok: ok}, err
	}
	peerIP := gatewayPeerIP(request)
	if net.ParseIP(peerIP) == nil {
		return runtimeAuthentication{
			codespaceUUID: codespaceUUID,
			peerIP:        peerIP,
			ok:            true,
		}, nil
	}
	if s.sourceResolver == nil {
		return runtimeAuthentication{
			codespaceUUID:  codespaceUUID,
			peerIP:         peerIP,
			ok:             true,
			bindingMatched: true,
		}, nil
	}
	source, found, err := s.sourceResolver.ResolveRuntimeSource(request.Context(), peerIP)
	if err != nil {
		return runtimeAuthentication{}, err
	}
	return runtimeAuthentication{
		codespaceUUID:  codespaceUUID,
		peerIP:         peerIP,
		ok:             true,
		bindingMatched: found && source.CodespaceUUID == codespaceUUID,
	}, nil
}

func (s *runtimeAPIService) handleGetEndpoint(writer http.ResponseWriter, codespaceUUID, endpointID string) {
	route, ok, err := s.loadEndpointRoute(codespaceUUID, endpointID)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if !ok {
		writeRuntimeAPIError(writer, http.StatusNotFound, "endpoint_not_found")
		return
	}
	response, err := runtimeEndpointResponseFromRoute(route)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *runtimeAPIService) handlePostEndpoint(writer http.ResponseWriter, request *http.Request, codespaceUUID, endpointID, peerIP string) {
	route, ok := s.runtimeEndpointRouteFromRequest(writer, request, codespaceUUID, endpointID, peerIP)
	if !ok {
		return
	}
	existing, exists, err := s.loadEndpointRoute(codespaceUUID, endpointID)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if exists {
		if sameGatewayEndpointRoute(existing, route) {
			response, err := runtimeEndpointResponseFromRoute(existing)
			if err != nil {
				writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
				return
			}
			writeJSON(writer, http.StatusOK, response)
			return
		}
		writeRuntimeAPIError(writer, http.StatusConflict, "endpoint_conflict")
		return
	}
	s.saveEndpointRoute(writer, request, route)
}

func (s *runtimeAPIService) handlePutEndpoint(writer http.ResponseWriter, request *http.Request, codespaceUUID, endpointID, peerIP string) {
	route, ok := s.runtimeEndpointRouteFromRequest(writer, request, codespaceUUID, endpointID, peerIP)
	if !ok {
		return
	}
	existing, exists, err := s.loadEndpointRoute(codespaceUUID, endpointID)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if !exists {
		writeRuntimeAPIError(writer, http.StatusNotFound, "endpoint_not_found")
		return
	}
	if sameGatewayEndpointRoute(existing, route) {
		response, err := runtimeEndpointResponseFromRoute(existing)
		if err != nil {
			writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
			return
		}
		writeJSON(writer, http.StatusOK, response)
		return
	}
	s.saveEndpointRoute(writer, request, route)
}

func (s *runtimeAPIService) handleDeleteEndpoint(writer http.ResponseWriter, request *http.Request, codespaceUUID, endpointID string) {
	if err := s.state.DeleteEndpointRoute(codespaceUUID, endpointID); err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	s.routes.Delete(codespaceUUID, endpointID)
	if !s.reportRuntimeMetadata(writer, request.Context(), codespaceUUID) {
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *runtimeAPIService) runtimeEndpointRouteFromRequest(
	writer http.ResponseWriter,
	request *http.Request,
	codespaceUUID string,
	endpointID string,
	peerIP string,
) (gatewayEndpointRoute, bool) {
	var payload runtimeEndpointRequest
	if !decodeRuntimeJSON(writer, request, &payload) {
		return gatewayEndpointRoute{}, false
	}
	public, ok := runtimeEndpointPublic(payload.Public)
	if !ok {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return gatewayEndpointRoute{}, false
	}
	if endpointID == "workspace" && public {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return gatewayEndpointRoute{}, false
	}
	if payload.UpstreamPort < 1 || payload.UpstreamPort > 65535 {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return gatewayEndpointRoute{}, false
	}
	route, err := normalizeGatewayEndpointRoute(gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     endpointID,
		label:          payload.Label,
		upstreamScheme: payload.UpstreamScheme,
		upstreamHost:   net.JoinHostPort(peerIP, strconv.Itoa(payload.UpstreamPort)),
		public:         public,
	})
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return gatewayEndpointRoute{}, false
	}
	if err := validateEndpointLabel(route.label); err != nil {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return gatewayEndpointRoute{}, false
	}
	return route, true
}

func (s *runtimeAPIService) saveEndpointRoute(writer http.ResponseWriter, request *http.Request, route gatewayEndpointRoute) {
	if err := s.state.SaveEndpointRoute(route); err != nil {
		if errors.Is(err, errEndpointLimitExceeded) {
			writeRuntimeAPIError(writer, http.StatusTooManyRequests, "endpoint_limit_exceeded")
			return
		}
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if err := s.routes.Put(route); err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	if !s.reportRuntimeMetadata(writer, request.Context(), route.codespaceUUID) {
		return
	}
	response, err := runtimeEndpointResponseFromRoute(route)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *runtimeAPIService) reportRuntimeMetadata(writer http.ResponseWriter, ctx context.Context, codespaceUUID string) bool {
	if s.controlPlane == nil {
		return true
	}
	generation, metadataJSON, ok, err := s.state.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		writeRuntimeAPIError(writer, http.StatusServiceUnavailable, "runtime_unavailable")
		return false
	}
	if !ok {
		return true
	}
	if err := s.controlPlane.reportRuntimeMetadata(ctx, codespaceUUID, metadataJSON, generation); err != nil {
		return true
	}
	return true
}

func (s *runtimeAPIService) loadEndpointRoute(codespaceUUID, endpointID string) (gatewayEndpointRoute, bool, error) {
	routes, err := s.state.LoadGatewayRoutes()
	if err != nil {
		return gatewayEndpointRoute{}, false, err
	}
	for _, route := range routes {
		if route.codespaceUUID == codespaceUUID && route.endpointID == endpointID {
			return route, true, nil
		}
	}
	return gatewayEndpointRoute{}, false, nil
}

func runtimeEndpointIDFromPath(path string) (string, bool) {
	endpointID, ok := strings.CutPrefix(path, runtimeEndpointAPIPrefix)
	if !ok || endpointID == "" || strings.Contains(endpointID, "/") {
		return "", false
	}
	if endpointID != "workspace" && !isGatewayEndpointID(endpointID) {
		return "", false
	}
	return endpointID, true
}

func decodeRuntimeJSON(writer http.ResponseWriter, request *http.Request, value any) bool {
	contentType := strings.TrimSpace(request.Header.Get("Content-Type"))
	if contentType == "" {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeRuntimeAPIError(writer, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func runtimeBearerToken(request *http.Request) (string, bool) {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	token, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", false
	}
	return token, true
}

func runtimeEndpointPublic(value *bool) (bool, bool) {
	if value == nil {
		return false, false
	}
	return *value, true
}

func runtimeEndpointResponseFromRoute(route gatewayEndpointRoute) (runtimeEndpointResponse, error) {
	_, port, err := net.SplitHostPort(route.upstreamHost)
	if err != nil {
		return runtimeEndpointResponse{}, fmt.Errorf("parse upstream host: %w", err)
	}
	portValue, err := strconv.Atoi(port)
	if err != nil {
		return runtimeEndpointResponse{}, fmt.Errorf("parse upstream port: %w", err)
	}
	return runtimeEndpointResponse{
		EndpointID:     route.endpointID,
		Label:          route.label,
		UpstreamScheme: route.upstreamScheme,
		UpstreamPort:   portValue,
		Public:         route.public,
	}, nil
}

func sameGatewayEndpointRoute(left, right gatewayEndpointRoute) bool {
	return left.codespaceUUID == right.codespaceUUID &&
		left.endpointID == right.endpointID &&
		left.label == right.label &&
		left.upstreamScheme == right.upstreamScheme &&
		left.upstreamHost == right.upstreamHost &&
		left.public == right.public
}

func writeRuntimeAPIError(writer http.ResponseWriter, statusCode int, category string) {
	if category == "" {
		category = "runtime_unavailable"
	}
	writeJSON(writer, statusCode, map[string]any{"error": map[string]any{"category": category}})
}
