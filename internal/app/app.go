// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/signal"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
)

const gatewaySessionCookieName = "gitea_codespace_session"

const (
	gatewayHTTPMaxHeaderBytes = 64 * 1024
	gatewayHTTPReadHeaderTime = 10 * time.Second
)

// Run starts the Codespace Manager process.
func Run(output io.Writer) error {
	if output == nil {
		return fmt.Errorf("output is nil")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runService(ctx, output)
}

// RunGateway starts network access using the shared external deployment state.
func RunGateway(ctx context.Context, output io.Writer) (resultErr error) {
	if output == nil {
		return fmt.Errorf("output is nil")
	}
	store, err := openEtcdInfrastructureStore()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	loadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	config, err := store.LoadRuntimeConfig(loadCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("load gateway configuration: %w", err)
	}
	config.store = store
	return runGatewayConfig(ctx, output, config)
}

func runManagerRuntime(ctx context.Context, output io.Writer, runtimeConfig InfrastructureRuntimeConfig) error {
	if output == nil || runtimeConfig.store == nil {
		return fmt.Errorf("process output and deployment store are required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	output = &processOutput{writer: output}
	done := make(chan error, 2)
	go func() { done <- runLeaderLoop(ctx, output, runtimeConfig) }()
	go func() { done <- runGatewayConfig(ctx, output, runtimeConfig) }()
	err := <-done
	cancel()
	return errors.Join(err, <-done)
}

type processOutput struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *processOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

func runGatewayConfig(ctx context.Context, output io.Writer, runtimeConfig InfrastructureRuntimeConfig) error {
	config := runtimeConfig.Config
	state, err := loadProcessState(runtimeConfig, nil)
	if err != nil {
		return err
	}
	state.gatewaySSHHostKey, err = runtimeConfig.store.LoadGatewaySSHHostKey(ctx)
	if err != nil {
		return err
	}
	runtime, err := newProcessRuntime(ctx, config, state, nil, runtimeConfig.store, false)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listeners, err := openGatewayListeners(config.Gateway)
	if err != nil {
		runtime.gatewayRoutes.Close()
		return err
	}
	defer listeners.Close()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		runtimeConfig.store.WatchGatewayRuntimes(ctx, runtime.gatewayRoutes)
	}()
	activityDone := make(chan struct{})
	go func() {
		defer close(activityDone)
		runtimeConfig.store.publishGatewayActivity(ctx, runtime.sessions)
	}()
	errorsCh := make(chan error, 2)
	var servers sync.WaitGroup
	servers.Add(2)
	go func() {
		defer servers.Done()
		serveHTTP(ctx, errorsCh, "gateway http", runtime.gatewayServer, listeners.GatewayHTTP)
	}()
	go func() {
		defer servers.Done()
		serveSSH(ctx, errorsCh, listeners.GatewaySSH, runtime.gatewaySSHServer)
	}()
	_, _ = fmt.Fprintf(output, "codespace gateway http listening on %s\n", listeners.GatewayHTTP.Addr())
	_, _ = fmt.Fprintf(output, "codespace gateway ssh listening on %s\n", listeners.GatewaySSH.Addr())
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
	}
	cancel()
	listeners.Close()
	<-watchDone
	<-activityDone
	runtime.processHealth.Fail()
	runtime.gatewayRoutes.Close()
	shutdown, stop := context.WithTimeout(context.Background(), config.Node.ShutdownTimeout.ToStdlib())
	defer stop()
	if closeErr := runtime.gatewayServer.Shutdown(shutdown); closeErr != nil {
		err = errors.Join(err, closeErr, runtime.gatewayServer.Close())
	}
	serversDone := make(chan struct{})
	go func() { servers.Wait(); close(serversDone) }()
	select {
	case <-serversDone:
	case <-shutdown.Done():
		err = errors.Join(err, shutdown.Err())
	}
	return err
}

func validateProcessRuntimeBindings(ctx context.Context, state processStateSnapshot, store managerInfrastructureStore) error {
	if store == nil {
		return nil
	}
	bindings, err := store.ListRuntimeBindings(ctx)
	if err != nil {
		return fmt.Errorf("load runtime bindings: %w", err)
	}
	byUUID := make(map[string]RuntimeBinding, len(bindings))
	for _, binding := range bindings {
		byUUID[binding.RuntimeUUID] = binding
	}
	for _, site := range state.sites {
		cleanupPending := make(map[string]struct{}, len(site.initialCleanupPendings))
		for _, runtimeUUID := range site.initialCleanupPendings {
			cleanupPending[runtimeUUID] = struct{}{}
		}
		for _, runtimeUUID := range site.runtimeUUIDs {
			if _, ok := cleanupPending[runtimeUUID]; ok {
				continue
			}
			binding, ok := byUUID[runtimeUUID]
			if !ok {
				return fmt.Errorf("runtime %s in site %d state has no runtime binding", runtimeUUID, site.site.ID)
			}
			if binding.SiteID != site.site.ID {
				return fmt.Errorf("runtime %s state belongs to site %d but binding belongs to site %d", runtimeUUID, site.site.ID, binding.SiteID)
			}
		}
	}
	return nil
}

type processStateSnapshot struct {
	sites             []processSiteState
	gatewaySSHHostKey gatewaySSHHostKey
}

type processSiteState struct {
	site                      ManagerSite
	codespaceStateStore       *CodespaceStateStore
	initialOperations         []manager.OperationSnapshot
	initialRuntimeGenerations map[string]int64
	initialRuntimeTransitions []manager.RuntimeTransitionSnapshot
	initialCleanupPendings    []string
	initialHealthStopPendings []manager.HealthStopSnapshot
	initialGatewayRoutes      []gatewayEndpointRoute
	runtimeUUIDs              []string
}

func loadProcessState(runtimeConfig InfrastructureRuntimeConfig, leader *deploymentLeadership) (processStateSnapshot, error) {
	sites := runtimeConfig.Sites
	if len(sites) == 0 {
		return processStateSnapshot{}, errInfrastructureStateEmpty
	}
	snapshot := processStateSnapshot{sites: make([]processSiteState, 0, len(sites))}
	for _, site := range sites {
		if leader == nil {
			snapshot.sites = append(snapshot.sites, processSiteState{site: site})
			continue
		}
		records := &executionStateStore{store: runtimeConfig.store, leader: leader, siteID: site.ID}
		generation, _, err := loadInventoryGeneration(records)
		if err != nil {
			return processStateSnapshot{}, err
		}
		site.InventoryGeneration = generation
		recovered, err := NewCodespaceStateStore(records).Recover()
		if err != nil {
			return processStateSnapshot{}, fmt.Errorf("recover site %d state: %w", site.ID, err)
		}
		recovered.site = site
		for i := range recovered.initialGatewayRoutes {
			recovered.initialGatewayRoutes[i].siteID = site.ID
		}
		snapshot.sites = append(snapshot.sites, recovered)
	}
	return snapshot, nil
}

type managerServiceSettingsStores []manager.ManagerServiceSettingsStore

func (stores managerServiceSettingsStores) SaveManagerServiceSettings(settings manager.ManagerServiceSettings) error {
	for _, store := range stores {
		if store == nil {
			continue
		}
		if err := store.SaveManagerServiceSettings(settings); err != nil {
			return err
		}
	}
	return nil
}

type processRuntime struct {
	sessions            *gatewaySessionRegistry
	sites               []processSiteRuntime
	processHealth       *processHealth
	gatewayServer       *http.Server
	gatewaySSHServer    *gatewaySSHServer
	capacityCoordinator manager.CapacityCoordinator
	gatewayRoutes       *gatewayRouteStore
}

type processSiteRuntime struct {
	siteID    int64
	agent     *manager.Agent
	publisher *runtimeMetadataPublisher
}

func registryCacheSecret(sites []ManagerSite) string {
	hash := sha256.New()
	for _, site := range sites {
		_, _ = fmt.Fprintf(hash, "%d\x00%s\x00", site.ID, site.ManagerSecret)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func newProcessRuntime(ctx context.Context, config Config, state processStateSnapshot, registryCache *registryCache, infrastructureStore managerInfrastructureStore, workerEnabled bool) (result *processRuntime, resultErr error) {
	var publishers []*runtimeMetadataPublisher
	sessionRegistry := newGatewaySessionRegistryFromConfig(config.Gateway)
	gatewayRoutes := newGatewayRouteStore()
	defer func() {
		if resultErr != nil {
			for _, publisher := range publishers {
				publisher.Close()
			}
			gatewayRoutes.Close()
		}
	}()
	gatewayRoutes.SetSessionRegistry(sessionRegistry)
	gatewayAccess := newGatewayAccessControllerFromConfig(config.Gateway)
	gatewayBrowserAuth := newGatewayBrowserAuth()
	gatewayOrigin, err := newGatewayOriginPolicy(config.Gateway.HTTP.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("configure gateway origin: %w", err)
	}
	controlPlanes := newGatewayControlPlanePool(gatewayRoutes)
	runtimeSites := make([]processSiteRuntime, 0, len(state.sites))
	capacityCoordinator := manager.NewSharedCapacityCoordinator(config.Node.CapacityTotal, config.Node.StartupWorkers, config.Node.CleanupWorkers)
	var sessionTracker manager.SessionTracker
	if workerEnabled {
		sessionTracker = &sharedGatewayActivity{store: state.sites[0].codespaceStateStore.records.store}
	}

	environments := make([]*codespacev1.EnvironmentTag, 0, len(config.Runtime.Environments))
	for _, environment := range config.Runtime.Environments {
		environments = append(environments, &codespacev1.EnvironmentTag{Tag: environment.Tag, Description: environment.Description})
	}
	sort.Slice(environments, func(i, j int) bool { return environments[i].GetTag() < environments[j].GetTag() })

	for _, siteState := range state.sites {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		site := siteState.site
		backendContext := ctx
		if workerEnabled {
			backendContext = siteState.codespaceStateStore.records.leader.session.Ctx()
		}
		managerProvisioner, err := newProvisioner(backendContext, config, site.ID, registryCache, workerEnabled)
		if err != nil {
			return nil, fmt.Errorf("create provisioner for site %d: %w", site.ID, err)
		}
		gatewayBackend, ok := managerProvisioner.(gatewayWorkspaceBackend)
		if !ok {
			return nil, fmt.Errorf("provisioner for site %d does not support gateway workspace access", site.ID)
		}
		if workerEnabled {
			siteState.codespaceStateStore.SetSessionRegistry(sessionRegistry)
		}
		gatewayRoutes.SetSiteBackend(site.ID, gatewayBackend)
		for _, route := range siteState.initialGatewayRoutes {
			if err := gatewayRoutes.Put(route); err != nil {
				return nil, fmt.Errorf("load site %d gateway route %s/%s: %w", site.ID, route.codespaceUUID, route.endpointID, err)
			}
		}

		controlPlane := newGatewayControlPlane(
			managerServiceBaseURL(site.GiteaURL), site.ManagerID, site.ManagerSecret,
			&http.Client{Timeout: config.Node.HTTPTimeout.ToStdlib()},
		)
		controlPlanes.Add(site.ID, controlPlane)
		var agent *manager.Agent
		var publisher *runtimeMetadataPublisher
		if workerEnabled {
			publisher = newRuntimeMetadataPublisher(siteState.codespaceStateStore, controlPlane, managerProvisioner, 0)
			publishers = append(publishers, publisher)
			gatewaySnapshots := &gatewayRuntimeSnapshotPublisher{siteID: site.ID, state: siteState.codespaceStateStore, store: infrastructureStore}
			publisher.SetGatewaySnapshotPublisher(gatewaySnapshots)
			publisher.Run(ctx)
			settings := managerServiceSettingsStores{controlPlane, gatewayBrowserAuth, publisher}
			endpointApplier := &runtimeEndpointApplier{state: siteState.codespaceStateStore, publisher: publisher, snapshots: gatewaySnapshots}
			initialRuntimeUUIDs := make(map[string]struct{})
			for _, route := range siteState.initialGatewayRoutes {
				initialRuntimeUUIDs[route.codespaceUUID] = struct{}{}
			}
			for codespaceUUID := range initialRuntimeUUIDs {
				if err := gatewaySnapshots.Sync(ctx, codespaceUUID); err != nil {
					return nil, fmt.Errorf("publish recovered gateway runtime %s for site %d: %w", codespaceUUID, site.ID, err)
				}
			}
			agent = manager.New(manager.AgentConfig{
				BaseURL: managerServiceBaseURL(site.GiteaURL), ManagerID: site.ManagerID, ManagerSecret: site.ManagerSecret,
				Name: config.Node.Name, GatewayURL: config.Gateway.HTTP.PublicURL, GatewaySSHAddr: config.Gateway.SSH.PublicAddr,
				GatewaySSHHostKeyAlgo: state.gatewaySSHHostKey.algorithm, GatewaySSHHostKeySHA256: state.gatewaySSHHostKey.fingerprintSHA256,
				GatewaySSHHostKeyUnix: state.gatewaySSHHostKey.updatedUnix, Version: managerBuildVersion(), Environments: environments,
				PollInterval: config.Node.PollInterval.ToStdlib(), DeclareInterval: config.Node.DeclareInterval.ToStdlib(),
				CapacityTotal: config.Node.CapacityTotal, StartupWorkers: config.Node.StartupWorkers, CleanupWorkers: config.Node.CleanupWorkers,
				CapacitySiteID: site.ID, CapacityCoordinator: capacityCoordinator,
				HTTPTimeout: config.Node.HTTPTimeout.ToStdlib(), ShutdownTimeout: config.Node.ShutdownTimeout.ToStdlib(), RuntimeMetadataGeneration: 1, InventoryGeneration: site.InventoryGeneration,
				InitialRuntimeGenerations: siteState.initialRuntimeGenerations, InitialRuntimeTransitions: siteState.initialRuntimeTransitions,
				InitialCleanupPendings: siteState.initialCleanupPendings, InitialHealthStopPendings: siteState.initialHealthStopPendings,
				InitialOperations: siteState.initialOperations, OperationStateStore: siteState.codespaceStateStore,
				InventoryStateStore: &ManagerStateStore{records: siteState.codespaceStateStore.records}, RuntimeStateStore: siteState.codespaceStateStore,
				CleanupStateStore: siteState.codespaceStateStore, HealthStopStateStore: siteState.codespaceStateStore,
				RuntimeEnvironmentStateStore: siteState.codespaceStateStore, RuntimeMetadataStateStore: siteState.codespaceStateStore,
				StartupInputStateStore: siteState.codespaceStateStore, RuntimeEndpointApplier: endpointApplier,
				RuntimeHealthStateStore: siteState.codespaceStateStore, RuntimeMetadataPublisher: publisher,
				RuntimeIdentityStore: &siteRuntimeIdentityStore{store: infrastructureStore, siteID: site.ID},
				SessionTracker:       sessionTracker, AccessController: gatewayRoutes, ManagerServiceSettings: settings,
				ExecutionContext: siteState.codespaceStateStore.records.leader.session.Ctx(),
				GitSSHKeyType:    config.runtimeGitSSHKeyType(),
			}, &http.Client{Timeout: config.Node.HTTPTimeout.ToStdlib()}, managerProvisioner)
		}
		runtimeSites = append(runtimeSites, processSiteRuntime{siteID: site.ID, agent: agent, publisher: publisher})
	}
	if err := loadSharedGatewayRuntimes(ctx, infrastructureStore, gatewayRoutes); err != nil {
		return nil, err
	}

	if workerEnabled {
		return &processRuntime{sites: runtimeSites, gatewayRoutes: gatewayRoutes, capacityCoordinator: capacityCoordinator}, nil
	}
	processHealth := newProcessHealth()
	gatewayServer := newGatewayHTTPServer(newGatewayHandlerWithOriginAndBrowserAuth(
		processHealth,
		sessionRegistry,
		gatewayAccess,
		controlPlanes,
		gatewayOrigin,
		gatewayBrowserAuth,
		gatewayRoutes,
	))
	gatewaySSHServer, err := newGatewaySSHServer(state.gatewaySSHHostKey.signer, gatewayRoutes, gatewayRoutes, controlPlanes, sessionRegistry, gatewayAccess, config.Gateway)
	if err != nil {
		return nil, fmt.Errorf("create gateway ssh server: %w", err)
	}
	return &processRuntime{
		sessions:            sessionRegistry,
		gatewayRoutes:       gatewayRoutes,
		sites:               runtimeSites,
		processHealth:       processHealth,
		gatewayServer:       gatewayServer,
		gatewaySSHServer:    gatewaySSHServer,
		capacityCoordinator: capacityCoordinator,
	}, nil
}

type siteRuntimeIdentityStore struct {
	store  managerInfrastructureStore
	siteID int64
}

func (s *siteRuntimeIdentityStore) SaveRuntimeIdentity(ctx context.Context, codespaceUUID string, codespaceID, operationRVersion int64, environmentTag string) error {
	if s == nil || s.store == nil {
		return nil
	}
	return s.store.SaveRuntimeBinding(ctx, RuntimeBinding{
		RuntimeUUID: codespaceUUID, SiteID: s.siteID, BackendID: "incus", CodespaceID: codespaceID,
		OperationRVersion: operationRVersion, EnvironmentTag: environmentTag,
	})
}

func (s *siteRuntimeIdentityStore) DeleteRuntimeIdentity(ctx context.Context, codespaceUUID string) error {
	if s == nil || s.store == nil {
		return nil
	}
	return s.store.DeleteRuntimeBinding(ctx, codespaceUUID)
}

type gatewayListeners struct {
	GatewayHTTP net.Listener
	GatewaySSH  net.Listener
}

func openGatewayListeners(config GatewayConfig) (*gatewayListeners, error) {
	listeners := &gatewayListeners{}
	var err error
	defer func() {
		if err != nil {
			listeners.Close()
		}
	}()

	listeners.GatewayHTTP, err = net.Listen("tcp", config.HTTP.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen gateway http %s: %w", config.HTTP.Listen, err)
	}
	listeners.GatewaySSH, err = net.Listen("tcp", config.SSH.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen gateway ssh %s: %w", config.SSH.Listen, err)
	}
	return listeners, nil
}

func (l *gatewayListeners) Close() {
	if l == nil {
		return
	}
	if l.GatewayHTTP != nil {
		_ = l.GatewayHTTP.Close()
	}
	if l.GatewaySSH != nil {
		_ = l.GatewaySSH.Close()
	}
}

func serveHTTP(ctx context.Context, errorChannel chan<- error, name string, server *http.Server, listener net.Listener) {
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed && !errors.Is(err, net.ErrClosed) {
		errorChannel <- fmt.Errorf("%s listener: %w", name, err)
		return
	}
	if ctx.Err() == nil {
		errorChannel <- fmt.Errorf("%s listener stopped unexpectedly", name)
	}
}

func newProvisioner(ctx context.Context, config Config, siteID int64, registryCache *registryCache, workerEnabled bool) (provisioner.Provisioner, error) {
	switch config.provisionerKind {
	case "dummy":
		return provisioner.NewDummy(), nil
	case "", "incus":
		remote, unixSocket, err := incusEndpoint(config.Runtime.Incus.Endpoint)
		if err != nil {
			return nil, err
		}
		if !workerEnabled {
			return provisioner.NewIncusGateway(ctx, provisioner.IncusConfig{
				SiteID: siteID, Project: config.Runtime.Incus.Project.Name, Remote: remote, UnixSocket: unixSocket,
			})
		}
		var cacheOptions provisioner.RuntimeCacheOptionsFunc
		var buildRegistry string
		var mirrors map[string]string
		if registryCache != nil && registryCache.enabled {
			cacheOptions = registryCache.CacheOptions
			buildRegistry = registryCache.publicURL + "/cache"
			mirrors = make(map[string]string, len(registryCache.upstreams))
			for host := range registryCache.upstreams {
				mirrors[host] = registryCache.publicURL + "/mirror/" + host
			}
		}
		return provisioner.NewIncus(ctx, provisioner.IncusConfig{
			SiteID:              siteID,
			Project:             config.Runtime.Incus.Project.Name,
			ProjectManage:       config.Runtime.Incus.Project.Manage,
			Remote:              remote,
			UnixSocket:          unixSocket,
			StoragePool:         config.Runtime.Incus.Storage.Pool,
			NetworkName:         config.Runtime.Incus.Network.Name,
			NetworkManage:       config.Runtime.Incus.Network.Manage,
			RuntimeEnvironments: provisionerEnvironments(config.Runtime.Environments),
			RuntimeExecutable:   config.runtimeExecutable,
			CodeServerVersion:   config.Runtime.WebIDE.CodeServerVersion,
			BuildCacheRegistry:  buildRegistry,
			RegistryMirrors:     mirrors,
			RuntimeCacheOptions: cacheOptions,
		})
	default:
		return nil, fmt.Errorf("unknown internal provisioner kind %q", config.provisionerKind)
	}
}

func provisionerEnvironments(environments []EnvironmentConfig) map[string]provisioner.IncusEnvironmentConfig {
	result := make(map[string]provisioner.IncusEnvironmentConfig, len(environments))
	for _, environment := range environments {
		sourceType := "image"
		var sourceProject, sourceName string
		if environment.Source.Instance != nil {
			sourceType = "instance"
			sourceProject = strings.TrimSpace(environment.Source.Instance.Project)
			sourceName = strings.TrimSpace(environment.Source.Instance.Name)
		}
		tag := strings.TrimSpace(environment.Tag)
		result[tag] = provisioner.IncusEnvironmentConfig{
			Image:         strings.TrimSpace(environment.Source.Image),
			InstanceType:  normalizeEnvironmentType(environment.Type),
			CPU:           environment.Resources.CPU,
			MemoryLimit:   strings.TrimSpace(environment.Resources.Memory),
			RootDiskSize:  strings.TrimSpace(environment.Resources.RootDisk),
			Profiles:      append([]string(nil), environment.Profiles...),
			SourceType:    sourceType,
			SourceProject: sourceProject,
			SourceName:    sourceName,
		}
	}
	return result
}

func incusEndpoint(endpoint string) (remote, unixSocket string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", "", fmt.Errorf("parse Incus endpoint: %w", err)
	}
	if parsed.Scheme == "unix" {
		return "", parsed.Path, nil
	}
	return parsed.String(), "", nil
}

func managerBuildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "development"
}

type healthStatus int32

const (
	healthStatusPass healthStatus = iota
	healthStatusWarn
	healthStatusFail
)

type processHealth struct {
	status atomic.Int32
}

func newProcessHealth() *processHealth {
	health := &processHealth{}
	health.status.Store(int32(healthStatusPass))
	return health
}

func (h *processHealth) Warn() {
	h.status.CompareAndSwap(int32(healthStatusPass), int32(healthStatusWarn))
}

func (h *processHealth) Fail() {
	h.status.Store(int32(healthStatusFail))
}

func (h *processHealth) writeHealthz(writer http.ResponseWriter) {
	switch healthStatus(h.status.Load()) {
	case healthStatusWarn:
		writeJSON(writer, http.StatusOK, map[string]any{"status": "warn"})
	case healthStatusFail:
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"status": "fail"})
	default:
		writeJSON(writer, http.StatusOK, map[string]any{"status": "pass"})
	}
}
