// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

// An endpoint route binds an authorized public address to one site's Runtime.
type endpointRoute struct {
	SiteUID       string
	ResourceUID   string
	GiteaWebURL   string
	PodUID        string
	AgentAddress  string
	TargetVersion int64
	CodespaceUUID string
	EndpointID    string
	Label         string
	Public        bool
}

// The route store owns routes and their live connections, closing them when a
// newer Manager snapshot replaces the Runtime target.
type routeStore struct {
	mu          sync.RWMutex
	routes      map[gatewayRouteKey]*gatewayRouteEntry
	nextLeaseID int64
	backend     workspaceBackend
	runtimes    map[string]runtimeSnapshot
}

type gatewayRouteEntry struct {
	route     endpointRoute
	leases    map[int64]context.CancelFunc
	transport *http.Transport
}

type gatewayRouteKey struct {
	codespaceUUID string
	endpointID    string
}

func newRouteStore(backend workspaceBackend) *routeStore {
	return &routeStore{
		routes:   make(map[gatewayRouteKey]*gatewayRouteEntry),
		backend:  backend,
		runtimes: make(map[string]runtimeSnapshot),
	}
}

func (s *routeStore) Get(codespaceUUID, endpointID string) (endpointRoute, bool) {
	if s == nil {
		return endpointRoute{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.routes[gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID}]
	if !ok {
		return endpointRoute{}, false
	}
	return entry.route, true
}

func (s *routeStore) giteaWebURLForCodespace(codespaceUUID string) (string, bool) {
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

func (s *routeStore) RuntimeTarget(runtimeUUID string) (runtimeSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runtime, found := s.runtimes[runtimeUUID]
	return runtime, found
}

func (s *routeStore) Counts() (runtimes, routes int) {
	if s == nil {
		return 0, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.runtimes), len(s.routes)
}

func (s *routeStore) ReplaceSharedGatewayRuntimes(snapshots []runtimeSnapshot) error {
	if s == nil {
		return fmt.Errorf("gateway route store is nil")
	}
	routes := make(map[gatewayRouteKey]endpointRoute)
	runtimes := make(map[string]runtimeSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		if err := validateRuntimeSnapshot(snapshot); err != nil {
			return err
		}
		if _, exists := runtimes[snapshot.RuntimeUUID]; exists {
			return fmt.Errorf("gateway runtime %q is duplicated", snapshot.RuntimeUUID)
		}
		runtimes[snapshot.RuntimeUUID] = snapshot
		for _, endpoint := range snapshot.Endpoints {
			route := endpointRoute{
				SiteUID: snapshot.SiteUID, ResourceUID: snapshot.ResourceUID, GiteaWebURL: snapshot.GiteaWebURL, PodUID: snapshot.PodUID, AgentAddress: snapshot.AgentAddress,
				TargetVersion: snapshot.TargetVersion, CodespaceUUID: snapshot.RuntimeUUID,
				EndpointID: endpoint.EndpointID, Label: endpoint.Label, Public: endpoint.Public,
			}
			routes[gatewayRouteKey{codespaceUUID: snapshot.RuntimeUUID, endpointID: endpoint.EndpointID}] = route
		}
	}

	s.mu.Lock()
	runtimesAdded, runtimesUpdated, runtimesRemoved := 0, 0, 0
	for runtimeUUID, runtime := range runtimes {
		previous, found := s.runtimes[runtimeUUID]
		if !found {
			runtimesAdded++
		} else if !sameGatewayRuntimeRouting(previous, runtime) {
			runtimesUpdated++
		}
	}
	for runtimeUUID := range s.runtimes {
		if _, found := runtimes[runtimeUUID]; !found {
			runtimesRemoved++
		}
	}
	type routeChange struct {
		event    string
		current  endpointRoute
		previous endpointRoute
	}
	routeChanges := make([]routeChange, 0)
	routesAdded, routesUpdated, routesRemoved := 0, 0, 0
	for key, route := range routes {
		previous := s.routes[key]
		if previous == nil {
			routesAdded++
			routeChanges = append(routeChanges, routeChange{event: "gateway_route_added", current: route})
		} else if previous.route != route {
			routesUpdated++
			routeChanges = append(routeChanges, routeChange{event: "gateway_route_updated", current: route, previous: previous.route})
		}
	}
	for key, previous := range s.routes {
		if _, found := routes[key]; !found {
			routesRemoved++
			routeChanges = append(routeChanges, routeChange{event: "gateway_route_removed", previous: previous.route})
		}
	}
	if runtimesAdded+runtimesUpdated+runtimesRemoved+routesAdded+routesUpdated+routesRemoved == 0 {
		s.mu.Unlock()
		return nil
	}
	oldEntries := s.routes
	s.routes = make(map[gatewayRouteKey]*gatewayRouteEntry, len(routes))
	for key, route := range routes {
		if old := oldEntries[key]; old != nil && sameGatewayEndpointTarget(old.route, route) {
			old.route = route
			s.routes[key] = old
			delete(oldEntries, key)
			continue
		}
		s.routes[key] = &gatewayRouteEntry{route: route}
	}
	oldRuntimes := s.runtimes
	s.runtimes = runtimes
	var cancels []context.CancelFunc
	for _, entry := range oldEntries {
		cancels = append(cancels, entry.takeCancels()...)
	}
	s.mu.Unlock()
	gatewayLog(context.Background(), slog.LevelInfo, "gateway_routes_updated",
		slog.Int("runtime_count", len(runtimes)),
		slog.Int("runtimes_added", runtimesAdded),
		slog.Int("runtimes_updated", runtimesUpdated),
		slog.Int("runtimes_removed", runtimesRemoved),
		slog.Int("route_count", len(routes)),
		slog.Int("routes_added", routesAdded),
		slog.Int("routes_updated", routesUpdated),
		slog.Int("routes_removed", routesRemoved),
	)
	for _, change := range routeChanges {
		attrs := gatewayRouteLogAttrs(change.current)
		if change.event == "gateway_route_removed" {
			attrs = gatewayRouteLogAttrs(change.previous)
		} else if change.event == "gateway_route_updated" {
			attrs = append(attrs, slog.GroupAttrs("previous_target", gatewayRouteLogAttrs(change.previous)...))
		}
		gatewayLog(context.Background(), slog.LevelDebug, change.event, attrs...)
	}
	if closer, ok := s.backend.(interface{ ClosePod(string) }); ok {
		for runtimeUUID, oldRuntime := range oldRuntimes {
			newRuntime, found := runtimes[runtimeUUID]
			if !found || oldRuntime.PodUID != newRuntime.PodUID || oldRuntime.AgentAddress != newRuntime.AgentAddress {
				closer.ClosePod(oldRuntime.PodUID)
			}
		}
	}
	cancelGatewayRouteLeases(cancels)
	return nil
}

func (s *routeStore) OpenWorkspaceCommand(ctx context.Context, request workspaceCommandRequest) (workspaceCommandSession, error) {
	return s.backend.OpenWorkspaceCommand(ctx, request)
}

func (s *routeStore) OpenWorkspaceShell(ctx context.Context, request workspaceShellRequest) (workspaceCommandSession, error) {
	return s.backend.OpenWorkspaceShell(ctx, request)
}

func (s *routeStore) OpenWorkspaceSFTP(ctx context.Context, request workspaceSFTPRequest) (io.ReadWriteCloser, error) {
	return s.backend.OpenWorkspaceSFTP(ctx, request)
}

func (s *routeStore) OpenWorkspaceTCP(ctx context.Context, runtimeUUID string, port uint32) (net.Conn, error) {
	return s.backend.OpenWorkspaceTCP(ctx, runtimeUUID, port)
}

func (s *routeStore) OpenWorkspaceEndpoint(ctx context.Context, runtimeUUID, endpointID string) (net.Conn, error) {
	return s.backend.OpenWorkspaceEndpoint(ctx, runtimeUUID, endpointID)
}

func (s *routeStore) BeginProxy(request *http.Request, codespaceUUID, endpointID string) (endpointRoute, *http.Request, func(), bool) {
	if s == nil || request == nil {
		return endpointRoute{}, request, func() {}, false
	}
	ctx, cancel := context.WithCancel(request.Context())
	key := gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID}

	s.mu.Lock()
	entry, ok := s.routes[key]
	if !ok {
		s.mu.Unlock()
		cancel()
		return endpointRoute{}, request, func() {}, false
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

func (s *routeStore) Put(route endpointRoute) error {
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
	if oldEntry != nil && sameGatewayEndpointTarget(oldEntry.route, route) {
		previous := oldEntry.route
		oldEntry.route = route
		s.mu.Unlock()
		if previous != route {
			attrs := gatewayRouteLogAttrs(route)
			attrs = append(attrs, slog.GroupAttrs("previous_target", gatewayRouteLogAttrs(previous)...))
			gatewayLog(context.Background(), slog.LevelDebug, "gateway_route_updated", attrs...)
		}
		return nil
	}
	s.routes[key] = &gatewayRouteEntry{route: route}
	cancels := oldEntry.takeCancels()
	s.mu.Unlock()

	cancelGatewayRouteLeases(cancels)
	attrs := gatewayRouteLogAttrs(route)
	if oldEntry == nil {
		gatewayLog(context.Background(), slog.LevelDebug, "gateway_route_added", attrs...)
	} else {
		attrs = append(attrs, slog.GroupAttrs("previous_target", gatewayRouteLogAttrs(oldEntry.route)...))
		gatewayLog(context.Background(), slog.LevelDebug, "gateway_route_updated", attrs...)
	}
	return nil
}

func (s *routeStore) Delete(codespaceUUID, endpointID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	entry := s.routes[gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID}]
	delete(s.routes, gatewayRouteKey{codespaceUUID: codespaceUUID, endpointID: endpointID})
	cancels := entry.takeCancels()
	s.mu.Unlock()

	cancelGatewayRouteLeases(cancels)
	if entry != nil {
		gatewayLog(context.Background(), slog.LevelDebug, "gateway_route_removed", gatewayRouteLogAttrs(entry.route)...)
	}
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

func sameGatewayEndpointTarget(left, right endpointRoute) bool {
	return left.SiteUID == right.SiteUID &&
		left.ResourceUID == right.ResourceUID &&
		left.PodUID == right.PodUID &&
		left.AgentAddress == right.AgentAddress &&
		left.CodespaceUUID == right.CodespaceUUID &&
		left.EndpointID == right.EndpointID &&
		left.Public == right.Public
}

func sameGatewayRuntimeRouting(left, right runtimeSnapshot) bool {
	return left.SiteUID == right.SiteUID && left.ResourceUID == right.ResourceUID &&
		left.RuntimeUUID == right.RuntimeUUID && left.GiteaWebURL == right.GiteaWebURL &&
		left.PodUID == right.PodUID && left.AgentAddress == right.AgentAddress &&
		left.TargetVersion == right.TargetVersion
}

func normalizeEndpointRoute(route endpointRoute) (endpointRoute, error) {
	route.SiteUID = strings.TrimSpace(route.SiteUID)
	route.CodespaceUUID = strings.TrimSpace(route.CodespaceUUID)
	route.EndpointID = strings.TrimSpace(route.EndpointID)
	route.Label = strings.TrimSpace(route.Label)
	route.GiteaWebURL = strings.TrimRight(strings.TrimSpace(route.GiteaWebURL), "/")
	if route.SiteUID == "" {
		return endpointRoute{}, fmt.Errorf("site uid is required")
	}
	if route.CodespaceUUID == "" {
		return endpointRoute{}, fmt.Errorf("codespace uuid is required")
	}
	if route.GiteaWebURL == "" {
		return endpointRoute{}, fmt.Errorf("gitea web URL is required")
	}
	if route.EndpointID != runtimeendpoint.WorkspaceEndpointID && !isGatewayEndpointID(route.EndpointID) {
		return endpointRoute{}, fmt.Errorf("endpoint_id is invalid")
	}
	if route.EndpointID == runtimeendpoint.WorkspaceEndpointID &&
		(route.Label != runtimeendpoint.WorkspaceEndpointLabel || route.Public) {
		return endpointRoute{}, fmt.Errorf("workspace endpoint route is invalid")
	}
	return route, nil
}

func (s *routeStore) Transport(route endpointRoute) (*http.Transport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.routes[gatewayRouteKey{codespaceUUID: route.CodespaceUUID, endpointID: route.EndpointID}]
	if entry == nil || !sameGatewayEndpointTarget(entry.route, route) {
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

func (s *routeStore) Close() {
	s.mu.Lock()
	var cancels []context.CancelFunc
	for _, entry := range s.routes {
		cancels = append(cancels, entry.takeCancels()...)
	}
	s.routes = make(map[gatewayRouteKey]*gatewayRouteEntry)
	s.runtimes = make(map[string]runtimeSnapshot)
	backend := s.backend
	s.mu.Unlock()
	cancelGatewayRouteLeases(cancels)
	if closer, ok := backend.(interface{ Close() }); ok {
		closer.Close()
	}
}
