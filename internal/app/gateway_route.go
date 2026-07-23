// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
)

type gatewayEndpointRoute struct {
	codespaceUUID  string
	endpointID     string
	label          string
	upstreamScheme string
	upstreamHost   string
	public         bool
}

type gatewayRouteStore struct {
	mu     sync.RWMutex
	routes map[gatewayRouteKey]gatewayEndpointRoute
}

type gatewayRouteKey struct {
	codespaceUUID string
	endpointID    string
}

func newGatewayRouteStore() *gatewayRouteStore {
	return &gatewayRouteStore{
		routes: make(map[gatewayRouteKey]gatewayEndpointRoute),
	}
}

func (s *gatewayRouteStore) Get(codespaceUUID, endpointID string) (gatewayEndpointRoute, bool) {
	if s == nil {
		return gatewayEndpointRoute{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	route, ok := s.routes[gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID}]
	return route, ok
}

func (s *gatewayRouteStore) Put(route gatewayEndpointRoute) error {
	if s == nil {
		return fmt.Errorf("gateway route store is nil")
	}
	route, err := normalizeGatewayEndpointRoute(route)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.routes[gatewayRouteKey{codespaceUUID: route.codespaceUUID, endpointID: route.endpointID}] = route
	return nil
}

func (s *gatewayRouteStore) Delete(codespaceUUID, endpointID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.routes, gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID})
}

func normalizeGatewayEndpointRoute(route gatewayEndpointRoute) (gatewayEndpointRoute, error) {
	route.codespaceUUID = strings.TrimSpace(route.codespaceUUID)
	route.endpointID = strings.TrimSpace(route.endpointID)
	route.label = strings.TrimSpace(route.label)
	route.upstreamScheme = strings.ToLower(strings.TrimSpace(route.upstreamScheme))
	route.upstreamHost = strings.TrimSpace(route.upstreamHost)
	if route.codespaceUUID == "" {
		return gatewayEndpointRoute{}, fmt.Errorf("codespace uuid is required")
	}
	if route.endpointID != "workspace" && !isGatewayEndpointID(route.endpointID) {
		return gatewayEndpointRoute{}, fmt.Errorf("endpoint_id is invalid")
	}
	switch route.upstreamScheme {
	case "http", "https":
	default:
		return gatewayEndpointRoute{}, fmt.Errorf("upstream_scheme must be http or https")
	}
	if !isValidGatewayUpstreamHost(route.upstreamHost) {
		return gatewayEndpointRoute{}, fmt.Errorf("upstream host is invalid")
	}
	if route.endpointID == "workspace" && route.public {
		return gatewayEndpointRoute{}, fmt.Errorf("workspace endpoint cannot be public")
	}
	return route, nil
}

func isValidGatewayUpstreamHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.HasSuffix(host, ".") {
		return false
	}
	hostOnly, port, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	if portValue, parseErr := strconv.Atoi(port); parseErr != nil || portValue < 1 || portValue > 65535 {
		return false
	}
	if ip := net.ParseIP(hostOnly); ip != nil {
		return true
	}
	if hostOnly == "localhost" {
		return true
	}
	if strings.ContainsAny(hostOnly, "/\\") {
		return false
	}
	return hostOnly != ""
}
