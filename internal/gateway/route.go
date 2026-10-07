// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

// EndpointRoute binds an authorized public address to one site's runtime port.
type EndpointRoute struct {
	GiteaWebURL   string `json:"-"`
	PodUID        string `json:"-"`
	AgentAddress  string `json:"-"`
	TargetVersion int64  `json:"-"`
	CodespaceUUID string `json:"-"`
	EndpointID    string `json:"-"`
	Label         string `json:"-"`
	Public        bool   `json:"-"`
}

// RouteStore owns routes and their live connections, closing them on replacement.
type RouteStore struct {
	mu          sync.RWMutex
	routes      map[gatewayRouteKey]*gatewayRouteEntry
	nextLeaseID int64
	sessions    *SessionRegistry
	backend     WorkspaceBackend
	runtimes    map[string]RuntimeSnapshot
}

type gatewayRouteEntry struct {
	route     EndpointRoute
	leases    map[int64]context.CancelFunc
	transport *http.Transport
}

type gatewayRouteKey struct {
	codespaceUUID string
	endpointID    string
}

func NewRouteStore(backend WorkspaceBackend) *RouteStore {
	return &RouteStore{
		routes:   make(map[gatewayRouteKey]*gatewayRouteEntry),
		backend:  backend,
		runtimes: make(map[string]RuntimeSnapshot),
	}
}

func (s *RouteStore) Get(codespaceUUID, endpointID string) (EndpointRoute, bool) {
	if s == nil {
		return EndpointRoute{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.routes[gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID}]
	if !ok {
		return EndpointRoute{}, false
	}
	return entry.route, true
}

func (s *RouteStore) SetSessionRegistry(sessions *SessionRegistry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions = sessions
}

func (s *RouteStore) giteaWebURLForCodespace(codespaceUUID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runtime, ok := s.runtimes[codespaceUUID]
	if ok && runtime.GiteaWebURL != "" {
		return runtime.GiteaWebURL, true
	}
	for key, entry := range s.routes {
		if key.codespaceUUID == codespaceUUID && entry.route.GiteaWebURL != "" {
			return entry.route.GiteaWebURL, true
		}
	}
	return "", false
}

func (s *RouteStore) RuntimeAvailable(runtimeUUID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, found := s.runtimes[runtimeUUID]
	return found && s.backend != nil, nil
}

func (s *RouteStore) ReplaceSharedGatewayRuntimes(snapshots []RuntimeSnapshot) error {
	if s == nil {
		return fmt.Errorf("gateway route store is nil")
	}
	routes := make(map[gatewayRouteKey]EndpointRoute)
	runtimes := make(map[string]RuntimeSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		if err := validateRuntimeSnapshot(snapshot); err != nil {
			return err
		}
		if _, exists := runtimes[snapshot.RuntimeUUID]; exists {
			return fmt.Errorf("gateway runtime %q is duplicated", snapshot.RuntimeUUID)
		}
		runtimes[snapshot.RuntimeUUID] = snapshot
		for _, endpoint := range snapshot.Endpoints {
			route := EndpointRoute{
				GiteaWebURL: snapshot.GiteaWebURL, PodUID: snapshot.PodUID, AgentAddress: snapshot.AgentAddress,
				TargetVersion: snapshot.TargetVersion, CodespaceUUID: snapshot.RuntimeUUID,
				EndpointID: endpoint.EndpointID, Label: endpoint.Label, Public: endpoint.Public,
			}
			routes[gatewayRouteKey{codespaceUUID: snapshot.RuntimeUUID, endpointID: endpoint.EndpointID}] = route
		}
	}

	s.mu.Lock()
	unchanged := len(s.runtimes) == len(runtimes) && len(s.routes) == len(routes)
	if unchanged {
		for key, runtime := range runtimes {
			previous, ok := s.runtimes[key]
			if !ok || !sameGatewayRuntimeRouting(previous, runtime) {
				unchanged = false
				break
			}
		}
	}
	if unchanged {
		for key, route := range routes {
			if entry := s.routes[key]; entry == nil || entry.route != route {
				unchanged = false
				break
			}
		}
	}
	if unchanged {
		s.mu.Unlock()
		return nil
	}
	oldEntries := s.routes
	s.routes = make(map[gatewayRouteKey]*gatewayRouteEntry, len(routes))
	for key, route := range routes {
		if old := oldEntries[key]; old != nil && sameGatewayEndpointRouting(old.route, route) {
			old.route = route
			s.routes[key] = old
			delete(oldEntries, key)
			continue
		}
		s.routes[key] = &gatewayRouteEntry{route: route}
	}
	oldRuntimes := s.runtimes
	s.runtimes = runtimes
	sessions := s.sessions
	var cancels []context.CancelFunc
	for _, entry := range oldEntries {
		cancels = append(cancels, entry.takeCancels()...)
	}
	s.mu.Unlock()
	if closer, ok := s.backend.(interface{ ClosePod(string) }); ok {
		for runtimeUUID, oldRuntime := range oldRuntimes {
			newRuntime, found := runtimes[runtimeUUID]
			if !found || oldRuntime.PodUID != newRuntime.PodUID || oldRuntime.AgentAddress != newRuntime.AgentAddress {
				closer.ClosePod(oldRuntime.PodUID)
			}
		}
	}
	if sessions != nil {
		for key := range oldEntries {
			sessions.DeleteEndpoint(key.codespaceUUID, key.endpointID)
		}
	}
	cancelGatewayRouteLeases(cancels)
	return nil
}

func (s *RouteStore) OpenWorkspaceCommand(ctx context.Context, request WorkspaceCommandRequest) (WorkspaceCommandSession, error) {
	return s.backend.OpenWorkspaceCommand(ctx, request)
}

func (s *RouteStore) OpenWorkspaceSFTP(ctx context.Context, request WorkspaceSFTPRequest) (io.ReadWriteCloser, error) {
	return s.backend.OpenWorkspaceSFTP(ctx, request)
}

func (s *RouteStore) OpenWorkspaceTCP(ctx context.Context, runtimeUUID string, port uint32) (net.Conn, error) {
	return s.backend.OpenWorkspaceTCP(ctx, runtimeUUID, port)
}

func (s *RouteStore) OpenWorkspaceEndpoint(ctx context.Context, runtimeUUID, endpointID string) (net.Conn, error) {
	return s.backend.OpenWorkspaceEndpoint(ctx, runtimeUUID, endpointID)
}

func (s *RouteStore) BeginProxy(request *http.Request, codespaceUUID, endpointID string) (EndpointRoute, *http.Request, func(), bool) {
	if s == nil || request == nil {
		return EndpointRoute{}, request, func() {}, false
	}
	ctx, cancel := context.WithCancel(request.Context())
	key := gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID}

	s.mu.Lock()
	entry, ok := s.routes[key]
	if !ok {
		s.mu.Unlock()
		cancel()
		return EndpointRoute{}, request, func() {}, false
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

func (s *RouteStore) Put(route EndpointRoute) error {
	if s == nil {
		return fmt.Errorf("gateway route store is nil")
	}
	route, err := normalizeEndpointRoute(route)
	if err != nil {
		return err
	}

	key := gatewayRouteKey{codespaceUUID: route.CodespaceUUID, endpointID: route.EndpointID}
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
		sessions.DeleteEndpoint(route.CodespaceUUID, route.EndpointID)
	}
	cancelGatewayRouteLeases(cancels)
	return nil
}

func (s *RouteStore) Delete(codespaceUUID, endpointID string) {
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

func (e *gatewayRouteEntry) takeCancels() []context.CancelFunc {
	if e == nil {
		return nil
	}
	if e.transport != nil {
		e.transport.CloseIdleConnections()
		e.transport = nil
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

func sameGatewayEndpointRouting(left, right EndpointRoute) bool {
	return left.GiteaWebURL == right.GiteaWebURL &&
		left.PodUID == right.PodUID &&
		left.AgentAddress == right.AgentAddress &&
		left.CodespaceUUID == right.CodespaceUUID &&
		left.EndpointID == right.EndpointID &&
		left.Public == right.Public
}

func sameGatewayRuntimeRouting(left, right RuntimeSnapshot) bool {
	return left.SiteUID == right.SiteUID && left.ResourceUID == right.ResourceUID &&
		left.RuntimeUUID == right.RuntimeUUID && left.GiteaWebURL == right.GiteaWebURL &&
		left.PodUID == right.PodUID && left.AgentAddress == right.AgentAddress
}

func normalizeEndpointRoute(route EndpointRoute) (EndpointRoute, error) {
	route.CodespaceUUID = strings.TrimSpace(route.CodespaceUUID)
	route.EndpointID = strings.TrimSpace(route.EndpointID)
	route.Label = strings.TrimSpace(route.Label)
	route.GiteaWebURL = strings.TrimRight(strings.TrimSpace(route.GiteaWebURL), "/")
	if route.CodespaceUUID == "" {
		return EndpointRoute{}, fmt.Errorf("codespace uuid is required")
	}
	if route.GiteaWebURL == "" {
		return EndpointRoute{}, fmt.Errorf("gitea web URL is required")
	}
	if route.EndpointID != runtimeendpoint.WorkspaceEndpointID && !isGatewayEndpointID(route.EndpointID) {
		return EndpointRoute{}, fmt.Errorf("endpoint_id is invalid")
	}
	if route.EndpointID == runtimeendpoint.WorkspaceEndpointID &&
		(route.Label != runtimeendpoint.WorkspaceEndpointLabel || route.Public) {
		return EndpointRoute{}, fmt.Errorf("workspace endpoint route is invalid")
	}
	return route, nil
}

func (s *RouteStore) Transport(route EndpointRoute) (*http.Transport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.routes[gatewayRouteKey{codespaceUUID: route.CodespaceUUID, endpointID: route.EndpointID}]
	if entry == nil || !sameGatewayEndpointRouting(entry.route, route) {
		return nil, fmt.Errorf("gateway endpoint route changed")
	}
	if s.backend == nil {
		return nil, fmt.Errorf("gateway endpoint backend is unavailable")
	}
	if entry.transport == nil {
		entry.transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return s.backend.OpenWorkspaceEndpoint(ctx, route.CodespaceUUID, route.EndpointID)
			},
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 8,
		}
	}
	return entry.transport, nil
}

func (s *RouteStore) Close() {
	s.mu.Lock()
	var cancels []context.CancelFunc
	for _, entry := range s.routes {
		cancels = append(cancels, entry.takeCancels()...)
	}
	s.routes = make(map[gatewayRouteKey]*gatewayRouteEntry)
	s.runtimes = make(map[string]RuntimeSnapshot)
	backend := s.backend
	s.mu.Unlock()
	cancelGatewayRouteLeases(cancels)
	if closer, ok := backend.(interface{ Close() }); ok {
		closer.Close()
	}
}
