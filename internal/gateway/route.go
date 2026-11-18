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
	"sync"
	"time"
)

// An endpoint route binds an authorized public address to one site's Runtime.
type endpointRoute struct {
	SiteUID       string
	ResourceUID   string
	GiteaWebURL   string
	PodUID        string
	AgentAddress  string
	TargetVersion int64
	RuntimeKey    string
	EndpointID    string
	Label         string
	Public        bool
}

// The route store owns routes and their live connections, closing them when a
// newer Manager snapshot replaces the Runtime target.
type routeStore struct {
	mu       sync.RWMutex
	routes   map[gatewayRouteKey]*gatewayRouteEntry
	backend  workspaceBackend
	runtimes map[string]runtimeSnapshot
	aliases  map[string]string
}

type gatewayRouteEntry struct {
	route     endpointRoute
	ctx       context.Context
	cancel    context.CancelFunc
	transport *http.Transport
}

type gatewayRouteKey struct {
	runtimeKey string
	endpointID string
}

func newRouteStore(backend workspaceBackend) *routeStore {
	return &routeStore{
		routes:   make(map[gatewayRouteKey]*gatewayRouteEntry),
		backend:  backend,
		runtimes: make(map[string]runtimeSnapshot),
		aliases:  make(map[string]string),
	}
}

func (s *routeStore) Get(runtimeKey, endpointID string) (endpointRoute, bool) {
	if s == nil {
		return endpointRoute{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.routes[gatewayRouteKey{runtimeKey: runtimeKey, endpointID: endpointID}]
	if !ok {
		return endpointRoute{}, false
	}
	return entry.route, true
}

func (s *routeStore) RuntimeTarget(runtimeKey string) (runtimeSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runtime, found := s.runtimes[runtimeKey]
	return runtime, found
}

func (s *routeStore) ResolveAlias(alias string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.aliases[alias]
	return key, ok
}

func (s *routeStore) bindingForRequest(request *http.Request, policy originPolicy) (gatewayRouteKey, bool) {
	host, ok := policy.bindingForRequest(request)
	if !ok {
		return gatewayRouteKey{}, false
	}
	key, ok := s.ResolveAlias(host.resourceUID)
	return gatewayRouteKey{runtimeKey: key, endpointID: host.endpointID}, ok
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
	aliases := make(map[string]string, len(snapshots))
	for _, snapshot := range snapshots {
		if err := validateRuntimeSnapshot(snapshot); err != nil {
			return err
		}
		if _, exists := runtimes[snapshot.RuntimeKey]; exists {
			return fmt.Errorf("gateway runtime %q is duplicated", snapshot.RuntimeKey)
		}
		runtimes[snapshot.RuntimeKey] = snapshot
		if _, exists := aliases[snapshot.ResourceUID]; exists {
			return fmt.Errorf("gateway runtime alias %q is duplicated", snapshot.ResourceUID)
		}
		aliases[snapshot.ResourceUID] = snapshot.RuntimeKey
		for _, endpoint := range snapshot.Endpoints {
			route := endpointRoute{
				SiteUID: snapshot.SiteUID, ResourceUID: snapshot.ResourceUID, GiteaWebURL: snapshot.GiteaWebURL, PodUID: snapshot.PodUID, AgentAddress: snapshot.AgentAddress,
				TargetVersion: snapshot.TargetVersion, RuntimeKey: snapshot.RuntimeKey,
				EndpointID: endpoint.EndpointID, Label: endpoint.Label, Public: endpoint.Public,
			}
			routes[gatewayRouteKey{runtimeKey: snapshot.RuntimeKey, endpointID: endpoint.EndpointID}] = route
		}
	}

	s.mu.Lock()
	runtimesAdded, runtimesUpdated, runtimesRemoved := 0, 0, 0
	for runtimeKey, runtime := range runtimes {
		previous, found := s.runtimes[runtimeKey]
		if !found {
			runtimesAdded++
		} else if !sameGatewayRuntimeRouting(previous, runtime) {
			runtimesUpdated++
		}
	}
	for runtimeKey := range s.runtimes {
		if _, found := runtimes[runtimeKey]; !found {
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
		ctx, cancel := context.WithCancel(context.Background())
		s.routes[key] = &gatewayRouteEntry{route: route, ctx: ctx, cancel: cancel}
	}
	oldRuntimes := s.runtimes
	s.runtimes = runtimes
	s.aliases = aliases
	s.mu.Unlock()
	for _, entry := range oldEntries {
		entry.close()
	}
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
		for runtimeKey, oldRuntime := range oldRuntimes {
			newRuntime, found := runtimes[runtimeKey]
			if !found || oldRuntime.PodUID != newRuntime.PodUID || oldRuntime.AgentAddress != newRuntime.AgentAddress || oldRuntime.SiteUID != newRuntime.SiteUID || oldRuntime.ResourceUID != newRuntime.ResourceUID {
				closer.ClosePod(oldRuntime.PodUID)
			}
		}
	}
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

func (s *routeStore) OpenWorkspaceTCP(ctx context.Context, runtimeKey string, port uint32) (net.Conn, error) {
	return s.backend.OpenWorkspaceTCP(ctx, runtimeKey, port)
}

func (s *routeStore) OpenWorkspaceReverseTCP(ctx context.Context, runtimeKey, address string, port uint32) (workspaceReverseListener, error) {
	return s.backend.OpenWorkspaceReverseTCP(ctx, runtimeKey, address, port)
}

func (s *routeStore) OpenWorkspaceEndpoint(ctx context.Context, runtimeKey, endpointID string) (net.Conn, error) {
	return s.backend.OpenWorkspaceEndpoint(ctx, runtimeKey, endpointID)
}

func (s *routeStore) BeginProxy(request *http.Request, runtimeKey, endpointID string) (endpointRoute, *http.Request, func(), bool) {
	if s == nil || request == nil {
		return endpointRoute{}, request, func() {}, false
	}
	ctx, cancel := context.WithCancelCause(request.Context())
	key := gatewayRouteKey{runtimeKey: runtimeKey, endpointID: endpointID}

	s.mu.RLock()
	entry, ok := s.routes[key]
	if !ok {
		s.mu.RUnlock()
		cancel(context.Canceled)
		return endpointRoute{}, request, func() {}, false
	}
	stop := context.AfterFunc(entry.ctx, func() { cancel(fmt.Errorf("gateway endpoint route changed")) })
	route := entry.route
	s.mu.RUnlock()

	release := func() {
		stop()
		cancel(context.Canceled)
	}
	return route, request.WithContext(ctx), release, true
}

func (e *gatewayRouteEntry) close() {
	if e == nil {
		return
	}
	e.cancel()
	if e.transport != nil {
		e.transport.CloseIdleConnections()
	}
}

func sameGatewayEndpointTarget(left, right endpointRoute) bool {
	return left.SiteUID == right.SiteUID &&
		left.ResourceUID == right.ResourceUID &&
		left.PodUID == right.PodUID &&
		left.AgentAddress == right.AgentAddress &&
		left.RuntimeKey == right.RuntimeKey &&
		left.EndpointID == right.EndpointID &&
		left.Public == right.Public
}

func sameGatewayRuntimeRouting(left, right runtimeSnapshot) bool {
	return left.SiteUID == right.SiteUID && left.ResourceUID == right.ResourceUID &&
		left.RuntimeKey == right.RuntimeKey && left.GiteaWebURL == right.GiteaWebURL &&
		left.PodUID == right.PodUID && left.AgentAddress == right.AgentAddress &&
		left.TargetVersion == right.TargetVersion
}

func (s *routeStore) Transport(route endpointRoute) (*http.Transport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.routes[gatewayRouteKey{runtimeKey: route.RuntimeKey, endpointID: route.EndpointID}]
	if entry == nil || !sameGatewayEndpointTarget(entry.route, route) {
		return nil, fmt.Errorf("gateway endpoint route changed")
	}
	if s.backend == nil {
		return nil, fmt.Errorf("gateway endpoint backend is unavailable")
	}
	if entry.transport == nil {
		entry.transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				ctx, cancel := context.WithCancel(ctx)
				stop := context.AfterFunc(entry.ctx, cancel)
				closeContext := func() { stop(); cancel() }
				conn, err := s.backend.OpenWorkspaceEndpoint(ctx, route.RuntimeKey, route.EndpointID)
				if err != nil {
					closeContext()
					return nil, err
				}
				return &routeConn{Conn: conn, cancel: closeContext}, nil
			},
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 8,
		}
	}
	return entry.transport, nil
}

// The transport owns the pooled connection; the route can also invalidate it.
type routeConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *routeConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

func (c *routeConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return c.Close()
}

func (s *routeStore) Close() {
	s.mu.Lock()
	entries := s.routes
	s.routes = make(map[gatewayRouteKey]*gatewayRouteEntry)
	s.runtimes = make(map[string]runtimeSnapshot)
	s.aliases = make(map[string]string)
	backend := s.backend
	s.mu.Unlock()
	for _, entry := range entries {
		entry.close()
	}
	if closer, ok := backend.(interface{ Close() }); ok {
		closer.Close()
	}
}
