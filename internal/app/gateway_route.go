// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

type gatewayEndpointRoute struct {
	siteID        int64
	codespaceUUID string
	endpointID    string
	label         string
	instanceName  string
	upstreamPort  uint32
	public        bool
}

type gatewayRouteStore struct {
	mu          sync.RWMutex
	routes      map[gatewayRouteKey]*gatewayRouteEntry
	nextLeaseID int64
	sessions    *gatewaySessionRegistry
	backends    map[int64]gatewayWorkspaceBackend
	targets     map[string]gatewayWorkspaceTarget
}

type gatewayRouteEntry struct {
	route     gatewayEndpointRoute
	leases    map[int64]context.CancelFunc
	transport *http.Transport
}

type gatewayRouteKey struct {
	codespaceUUID string
	endpointID    string
}

func newGatewayRouteStore() *gatewayRouteStore {
	return &gatewayRouteStore{
		routes:   make(map[gatewayRouteKey]*gatewayRouteEntry),
		backends: make(map[int64]gatewayWorkspaceBackend),
		targets:  make(map[string]gatewayWorkspaceTarget),
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

func (s *gatewayRouteStore) SetSiteBackend(siteID int64, backend gatewayWorkspaceBackend) {
	if s == nil || siteID <= 0 || backend == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backends[siteID] = backend
}

func (s *gatewayRouteStore) siteForCodespace(codespaceUUID string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, entry := range s.routes {
		if key.codespaceUUID == codespaceUUID {
			return entry.route.siteID, true
		}
	}
	return 0, false
}

func (s *gatewayRouteStore) backendForInstance(instanceName string) (gatewayWorkspaceBackend, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, entry := range s.routes {
		if entry.route.instanceName == instanceName {
			backend := s.backends[entry.route.siteID]
			if backend != nil {
				return backend, nil
			}
		}
	}
	return nil, fmt.Errorf("gateway backend for runtime instance is unavailable")
}

func (s *gatewayRouteStore) LoadGatewayWorkspaceTarget(codespaceUUID string) (gatewayWorkspaceTarget, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	target, found := s.targets[codespaceUUID]
	return target, found, nil
}

func (s *gatewayRouteStore) ReplaceSharedGatewayRuntimes(snapshots []GatewayRuntimeSnapshot) error {
	if s == nil {
		return fmt.Errorf("gateway route store is nil")
	}
	routes := make(map[gatewayRouteKey]gatewayEndpointRoute)
	targets := make(map[string]gatewayWorkspaceTarget, len(snapshots))
	for _, snapshot := range snapshots {
		if err := validateGatewayRuntimeSnapshot(snapshot); err != nil {
			return err
		}
		targets[snapshot.RuntimeUUID] = gatewayWorkspaceTarget{
			instanceName: snapshot.InstanceName, workdir: snapshot.Workdir, uid: snapshot.UID, gid: snapshot.GID,
			containerID: snapshot.ContainerID, containerUser: snapshot.ContainerUser,
			containerWorkdir: snapshot.ContainerWorkdir, editorPort: snapshot.EditorPort,
		}
		for _, endpoint := range snapshot.Endpoints {
			route := gatewayEndpointRoute{
				siteID: snapshot.SiteID, codespaceUUID: snapshot.RuntimeUUID, endpointID: endpoint.EndpointID,
				label: endpoint.Label, instanceName: snapshot.InstanceName, upstreamPort: endpoint.UpstreamPort, public: endpoint.Public,
			}
			routes[gatewayRouteKey{codespaceUUID: snapshot.RuntimeUUID, endpointID: endpoint.EndpointID}] = route
		}
	}

	s.mu.Lock()
	unchanged := maps.Equal(s.targets, targets) && len(s.routes) == len(routes)
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
	s.targets = targets
	sessions := s.sessions
	var cancels []context.CancelFunc
	for _, entry := range oldEntries {
		cancels = append(cancels, entry.takeCancels()...)
	}
	s.mu.Unlock()
	if sessions != nil {
		for key := range oldEntries {
			sessions.DeleteEndpoint(key.codespaceUUID, key.endpointID)
		}
	}
	cancelGatewayRouteLeases(cancels)
	return nil
}

func (s *gatewayRouteStore) OpenWorkspaceCommand(ctx context.Context, request provisioner.WorkspaceCommandRequest) (provisioner.WorkspaceCommandSession, error) {
	backend, err := s.backendForInstance(request.InstanceName)
	if err != nil {
		return nil, err
	}
	return backend.OpenWorkspaceCommand(ctx, request)
}

func (s *gatewayRouteStore) OpenWorkspaceSFTP(ctx context.Context, request provisioner.WorkspaceSFTPRequest) (io.ReadWriteCloser, error) {
	backend, err := s.backendForInstance(request.InstanceName)
	if err != nil {
		return nil, err
	}
	return backend.OpenWorkspaceSFTP(ctx, request)
}

func (s *gatewayRouteStore) OpenWorkspaceTCP(ctx context.Context, instanceName string, port uint32) (net.Conn, error) {
	backend, err := s.backendForInstance(instanceName)
	if err != nil {
		return nil, err
	}
	return backend.OpenWorkspaceTCP(ctx, instanceName, port)
}

func (s *gatewayRouteStore) CheckWorkspaceAccess(ctx context.Context, instanceName, workdir string) error {
	backend, err := s.backendForInstance(instanceName)
	if err != nil {
		return err
	}
	return backend.CheckWorkspaceAccess(ctx, instanceName, workdir)
}

func (s *gatewayRouteStore) CheckDevContainer(ctx context.Context, instanceName string) error {
	backend, err := s.backendForInstance(instanceName)
	if err != nil {
		return err
	}
	return backend.CheckDevContainer(ctx, instanceName)
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
	sessions := s.sessions
	s.mu.Unlock()

	if sessions != nil {
		sessions.DeleteCodespace(codespaceUUID)
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

func sameGatewayEndpointRouting(left, right gatewayEndpointRoute) bool {
	return left.siteID == right.siteID &&
		left.codespaceUUID == right.codespaceUUID &&
		left.endpointID == right.endpointID &&
		left.instanceName == right.instanceName &&
		left.upstreamPort == right.upstreamPort &&
		left.public == right.public
}

func normalizeGatewayEndpointRoute(route gatewayEndpointRoute) (gatewayEndpointRoute, error) {
	route.codespaceUUID = strings.TrimSpace(route.codespaceUUID)
	route.endpointID = strings.TrimSpace(route.endpointID)
	route.label = strings.TrimSpace(route.label)
	route.instanceName = strings.TrimSpace(route.instanceName)
	if route.codespaceUUID == "" {
		return gatewayEndpointRoute{}, fmt.Errorf("codespace uuid is required")
	}
	if route.endpointID != runtimeendpoint.WorkspaceEndpointID && !isGatewayEndpointID(route.endpointID) {
		return gatewayEndpointRoute{}, fmt.Errorf("endpoint_id is invalid")
	}
	if route.instanceName == "" {
		return gatewayEndpointRoute{}, fmt.Errorf("runtime instance name is required")
	}
	if route.upstreamPort == 0 || route.upstreamPort > 65535 {
		return gatewayEndpointRoute{}, fmt.Errorf("upstream port is invalid")
	}
	if route.endpointID == runtimeendpoint.WorkspaceEndpointID &&
		(route.label != runtimeendpoint.WorkspaceEndpointLabel || route.upstreamPort != runtimeendpoint.WorkspaceEndpointPort || route.public) {
		return gatewayEndpointRoute{}, fmt.Errorf("workspace endpoint route is invalid")
	}
	return route, nil
}

func gatewayEndpointRouteFromManager(route manager.RuntimeEndpointRoute) (gatewayEndpointRoute, error) {
	return normalizeGatewayEndpointRoute(gatewayEndpointRoute{
		codespaceUUID: route.CodespaceUUID,
		endpointID:    route.EndpointID,
		label:         route.Label,
		instanceName:  route.InstanceName,
		upstreamPort:  route.UpstreamPort,
		public:        route.Public,
	})
}

func (s *gatewayRouteStore) Transport(route gatewayEndpointRoute) (*http.Transport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.routes[gatewayRouteKey{codespaceUUID: route.codespaceUUID, endpointID: route.endpointID}]
	if entry == nil || !sameGatewayEndpointRouting(entry.route, route) {
		return nil, fmt.Errorf("gateway endpoint route changed")
	}
	backend := s.backends[route.siteID]
	if backend == nil {
		return nil, fmt.Errorf("gateway endpoint backend is unavailable")
	}
	if entry.transport == nil {
		entry.transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return backend.OpenWorkspaceTCP(ctx, route.instanceName, route.upstreamPort)
			},
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 8,
		}
	}
	return entry.transport, nil
}

func (s *gatewayRouteStore) Close() {
	s.mu.Lock()
	var cancels []context.CancelFunc
	for _, entry := range s.routes {
		cancels = append(cancels, entry.takeCancels()...)
	}
	s.routes = make(map[gatewayRouteKey]*gatewayRouteEntry)
	s.mu.Unlock()
	cancelGatewayRouteLeases(cancels)
}
