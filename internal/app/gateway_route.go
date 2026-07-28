// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"gitea.dev/codespace/internal/manager"
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
	mu              sync.RWMutex
	routes          map[gatewayRouteKey]*gatewayRouteEntry
	workspaceLeases map[string]map[int64]context.CancelFunc
	nextLeaseID     int64
	sessions        *gatewaySessionRegistry
	workspace       *gatewayWorkspaceIDE
}

type gatewayRouteEntry struct {
	route  gatewayEndpointRoute
	leases map[int64]context.CancelFunc
}

type gatewayRouteKey struct {
	codespaceUUID string
	endpointID    string
}

func newGatewayRouteStore() *gatewayRouteStore {
	return &gatewayRouteStore{
		routes:          make(map[gatewayRouteKey]*gatewayRouteEntry),
		workspaceLeases: make(map[string]map[int64]context.CancelFunc),
	}
}

func (s *gatewayRouteStore) Get(codespaceUUID, endpointID string) (gatewayEndpointRoute, bool) {
	if s == nil {
		return gatewayEndpointRoute{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.routes[gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID}]
	if !ok {
		return gatewayEndpointRoute{}, false
	}
	return entry.route, true
}

func (s *gatewayRouteStore) SetSessionRegistry(sessions *gatewaySessionRegistry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions = sessions
}

func (s *gatewayRouteStore) SetWorkspaceIDE(workspace *gatewayWorkspaceIDE) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.workspace = workspace
}

func (s *gatewayRouteStore) BeginProxy(request *http.Request, codespaceUUID, endpointID string) (gatewayEndpointRoute, *http.Request, func(), bool) {
	if s == nil || request == nil {
		return gatewayEndpointRoute{}, request, func() {}, false
	}
	ctx, cancel := context.WithCancel(request.Context())
	key := gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID}

	s.mu.Lock()
	entry, ok := s.routes[key]
	if !ok {
		s.mu.Unlock()
		cancel()
		return gatewayEndpointRoute{}, request, func() {}, false
	}
	s.nextLeaseID++
	leaseID := s.nextLeaseID
	if entry.leases == nil {
		entry.leases = make(map[int64]context.CancelFunc)
	}
	entry.leases[leaseID] = cancel
	route := entry.route
	s.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			s.mu.Lock()
			defer s.mu.Unlock()

			delete(entry.leases, leaseID)
		})
	}
	return route, request.WithContext(ctx), release, true
}

func (s *gatewayRouteStore) BeginWorkspaceIDE(request *http.Request, codespaceUUID string) (*gatewayWorkspaceIDE, *http.Request, func(), bool) {
	if s == nil || request == nil {
		return nil, request, func() {}, false
	}
	ctx, cancel := context.WithCancel(request.Context())

	s.mu.Lock()
	if s.workspace == nil {
		s.mu.Unlock()
		cancel()
		return nil, request, func() {}, false
	}
	s.nextLeaseID++
	leaseID := s.nextLeaseID
	if s.workspaceLeases[codespaceUUID] == nil {
		s.workspaceLeases[codespaceUUID] = make(map[int64]context.CancelFunc)
	}
	s.workspaceLeases[codespaceUUID][leaseID] = cancel
	workspace := s.workspace
	s.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			s.mu.Lock()
			defer s.mu.Unlock()

			delete(s.workspaceLeases[codespaceUUID], leaseID)
			if len(s.workspaceLeases[codespaceUUID]) == 0 {
				delete(s.workspaceLeases, codespaceUUID)
			}
		})
	}
	return workspace, request.WithContext(ctx), release, true
}

func (s *gatewayRouteStore) Put(route gatewayEndpointRoute) error {
	if s == nil {
		return fmt.Errorf("gateway route store is nil")
	}
	route, err := normalizeGatewayEndpointRoute(route)
	if err != nil {
		return err
	}

	key := gatewayRouteKey{codespaceUUID: route.codespaceUUID, endpointID: route.endpointID}
	s.mu.Lock()
	oldEntry := s.routes[key]
	if oldEntry != nil && sameGatewayEndpointRouting(oldEntry.route, route) {
		oldEntry.route = route
		s.mu.Unlock()
		return nil
	}
	s.routes[key] = &gatewayRouteEntry{route: route}
	sessions := s.sessions
	cancels := oldEntry.takeCancels()
	s.mu.Unlock()

	if oldEntry != nil && sessions != nil {
		sessions.DeleteEndpoint(route.codespaceUUID, route.endpointID)
	}
	cancelGatewayRouteLeases(cancels)
	return nil
}

func (s *gatewayRouteStore) ReplaceRuntimeEndpointRoutes(codespaceUUID string, routes []manager.RuntimeEndpointRoute) error {
	if s == nil {
		return fmt.Errorf("gateway route store is nil")
	}
	normalized := make(map[gatewayRouteKey]gatewayEndpointRoute, len(routes))
	for _, route := range routes {
		localRoute, err := gatewayEndpointRouteFromManager(route)
		if err != nil {
			return err
		}
		if localRoute.codespaceUUID != codespaceUUID {
			return fmt.Errorf("endpoint route codespace uuid mismatch")
		}
		key := gatewayRouteKey{codespaceUUID: localRoute.codespaceUUID, endpointID: localRoute.endpointID}
		if _, ok := normalized[key]; ok {
			return fmt.Errorf("duplicate endpoint_id %s", localRoute.endpointID)
		}
		normalized[key] = localRoute
	}

	s.mu.Lock()
	var cancelGroups [][]context.CancelFunc
	var sessionDeletes []gatewayRouteKey
	sessions := s.sessions
	for key, entry := range s.routes {
		if key.codespaceUUID != codespaceUUID {
			continue
		}
		next, keep := normalized[key]
		if keep && sameGatewayEndpointRouting(entry.route, next) {
			entry.route = next
			delete(normalized, key)
			continue
		}
		delete(s.routes, key)
		cancelGroups = append(cancelGroups, entry.takeCancels())
		sessionDeletes = append(sessionDeletes, key)
	}
	for key, route := range normalized {
		s.routes[key] = &gatewayRouteEntry{route: route}
		sessionDeletes = append(sessionDeletes, key)
	}
	s.mu.Unlock()

	if sessions != nil {
		for _, key := range sessionDeletes {
			sessions.DeleteEndpoint(key.codespaceUUID, key.endpointID)
		}
	}
	for _, cancels := range cancelGroups {
		cancelGatewayRouteLeases(cancels)
	}
	return nil
}

func (s *gatewayRouteStore) Delete(codespaceUUID, endpointID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	entry := s.routes[gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID}]
	delete(s.routes, gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID})
	sessions := s.sessions
	cancels := entry.takeCancels()
	s.mu.Unlock()

	if entry != nil && sessions != nil {
		sessions.DeleteEndpoint(codespaceUUID, endpointID)
	}
	cancelGatewayRouteLeases(cancels)
}

func (s *gatewayRouteStore) CloseCodespaceAccess(codespaceUUID string) {
	if s == nil || codespaceUUID == "" {
		return
	}
	s.mu.Lock()
	var cancels []context.CancelFunc
	for key, entry := range s.routes {
		if key.codespaceUUID != codespaceUUID {
			continue
		}
		cancels = append(cancels, entry.takeCancels()...)
	}
	cancels = append(cancels, s.takeWorkspaceCancels(codespaceUUID)...)
	sessions := s.sessions
	s.mu.Unlock()

	if sessions != nil {
		sessions.DeleteCodespace(codespaceUUID)
	}
	cancelGatewayRouteLeases(cancels)
}

func (s *gatewayRouteStore) takeWorkspaceCancels(codespaceUUID string) []context.CancelFunc {
	if s == nil || len(s.workspaceLeases[codespaceUUID]) == 0 {
		return nil
	}
	cancels := make([]context.CancelFunc, 0, len(s.workspaceLeases[codespaceUUID]))
	for _, cancel := range s.workspaceLeases[codespaceUUID] {
		cancels = append(cancels, cancel)
	}
	delete(s.workspaceLeases, codespaceUUID)
	return cancels
}

func (e *gatewayRouteEntry) takeCancels() []context.CancelFunc {
	if e == nil || len(e.leases) == 0 {
		return nil
	}
	cancels := make([]context.CancelFunc, 0, len(e.leases))
	for _, cancel := range e.leases {
		cancels = append(cancels, cancel)
	}
	e.leases = nil
	return cancels
}

func cancelGatewayRouteLeases(cancels []context.CancelFunc) {
	for _, cancel := range cancels {
		cancel()
	}
}

func sameGatewayEndpointRouting(left, right gatewayEndpointRoute) bool {
	return left.codespaceUUID == right.codespaceUUID &&
		left.endpointID == right.endpointID &&
		left.upstreamScheme == right.upstreamScheme &&
		left.upstreamHost == right.upstreamHost &&
		left.public == right.public
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
	if !isGatewayEndpointID(route.endpointID) {
		return gatewayEndpointRoute{}, fmt.Errorf("endpoint_id is invalid")
	}
	if route.endpointID == "workspace" {
		return gatewayEndpointRoute{}, fmt.Errorf("endpoint_id workspace is reserved for the platform Web IDE")
	}
	switch route.upstreamScheme {
	case "http", "https":
	default:
		return gatewayEndpointRoute{}, fmt.Errorf("upstream_scheme must be http or https")
	}
	if !isValidGatewayUpstreamHost(route.upstreamHost) {
		return gatewayEndpointRoute{}, fmt.Errorf("upstream host is invalid")
	}
	return route, nil
}

func gatewayEndpointRouteFromManager(route manager.RuntimeEndpointRoute) (gatewayEndpointRoute, error) {
	return normalizeGatewayEndpointRoute(gatewayEndpointRoute{
		codespaceUUID:  route.CodespaceUUID,
		endpointID:     route.EndpointID,
		label:          route.Label,
		upstreamScheme: route.UpstreamScheme,
		upstreamHost:   route.UpstreamHost,
		public:         route.Public,
	})
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
