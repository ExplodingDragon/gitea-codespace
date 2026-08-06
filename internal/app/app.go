// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

const gatewaySessionCookieName = "gitea_codespace_session"

const (
	gatewayHTTPMaxHeaderBytes = 64 * 1024
	gatewayHTTPReadHeaderTime = 10 * time.Second
)

// Run starts the Codespace Manager process.
func Run(output io.Writer, configPath string) error {
	if output == nil {
		return fmt.Errorf("output is nil")
	}

	infrastructureConfig, ok, err := LoadInfrastructureRuntimeConfig(configPath)
	if err != nil {
		return fmt.Errorf("load manager infrastructure state: %w", err)
	}
	if ok {
		return RunWithInfrastructureConfig(output, infrastructureConfig)
	}
	return fmt.Errorf("%s must be set to select the manager state backend", managerStateDriverEnv)
}

// RunWithInfrastructureConfig starts the Manager worker from manager-owned infrastructure state.
func RunWithInfrastructureConfig(output io.Writer, runtimeConfig InfrastructureRuntimeConfig) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runWithConfigContext(ctx, output, runtimeConfig)
}

func runWithConfigContext(ctx context.Context, output io.Writer, runtimeConfig InfrastructureRuntimeConfig) error {
	if output == nil {
		return fmt.Errorf("output is nil")
	}
	if err := validateManagerNodeRole(runtimeConfig.NodeRole); err != nil {
		return err
	}
	workerEnabled := strings.ToLower(strings.TrimSpace(runtimeConfig.NodeRole)) != managerNodeRoleGateway
	config := runtimeConfig.Config

	stateLock, err := acquireStateDirLock(config.Node.StateDir)
	if err != nil {
		return fmt.Errorf("acquire manager state dir lock: %w", err)
	}
	defer func() {
		if err := stateLock.Close(); err != nil {
			log.Printf("release manager state dir lock: %v", err)
		}
	}()

	state, err := loadProcessState(config, runtimeConfig.ManagerState)
	if err != nil {
		return err
	}

	registryCache, err := newRegistryCache(config, state.managerState.ManagerSecret)
	if err != nil {
		return fmt.Errorf("configure cache registry: %w", err)
	}
	listeners, err := openProcessListeners(config, registryCache)
	if err != nil {
		return err
	}
	defer listeners.Close()

	ctx, cancelProcess := context.WithCancel(ctx)
	defer cancelProcess()

	runtime, err := newProcessRuntime(ctx, config, state, registryCache, workerEnabled)
	if err != nil {
		return err
	}

	errorChannel := make(chan error, 3)
	go serveHTTP(ctx, errorChannel, "gateway http", runtime.gatewayServer, listeners.GatewayHTTP)
	go serveSSH(ctx, errorChannel, listeners.GatewaySSH, runtime.gatewaySSHServer)
	if listeners.RegistryCache != nil {
		go serveHTTP(ctx, errorChannel, "cache registry", runtime.registryCacheServer, listeners.RegistryCache)
		go registryCache.RunGC(ctx)
		_, _ = fmt.Fprintf(output, "codespace cache registry listening on %s\n", listeners.RegistryCache.Addr())
	}
	_, _ = fmt.Fprintf(output, "codespace gateway http listening on %s\n", listeners.GatewayHTTP.Addr())
	_, _ = fmt.Fprintf(output, "codespace gateway ssh listening on %s\n", listeners.GatewaySSH.Addr())
	if strings.TrimSpace(runtimeConfig.NodeID) != "" {
		_, _ = fmt.Fprintf(output, "codespace manager node %s\n", runtimeConfig.NodeID)
	}
	if !workerEnabled {
		_, _ = fmt.Fprintln(output, "codespace manager node role gateway")
	}
	_, _ = fmt.Fprintf(output, "codespace code-server version %s for new environments\n", config.Runtime.WebIDE.CodeServerVersion)

	if runtime.agent != nil {
		go func() {
			if err := runtime.agent.Run(ctx); err != nil {
				errorChannel <- fmt.Errorf("manager: %w", err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errorChannel:
		runErr = err
		cancelProcess()
	}
	runtime.processHealth.Fail()

	shutdownContext, cancel := context.WithTimeout(context.Background(), config.Node.ShutdownTimeout.ToStdlib())
	defer cancel()
	if err := runtime.gatewayServer.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shutdown gateway server: %w", err)
	}
	if runtime.registryCacheServer != nil {
		if err := runtime.registryCacheServer.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shutdown cache registry server: %w", err)
		}
	}
	listeners.Close()
	return runErr
}

type processStateSnapshot struct {
	managerState              ManagerState
	codespaceStateStore       *CodespaceStateStore
	initialOperations         []manager.OperationSnapshot
	initialRuntimeGenerations map[string]int64
	initialRuntimeTransitions []manager.RuntimeTransitionSnapshot
	initialCleanupPendings    []string
	initialHealthStopPendings []manager.HealthStopSnapshot
	initialGatewayRoutes      []gatewayEndpointRoute
	gatewaySSHHostKey         gatewaySSHHostKey
}

func loadProcessState(config Config, managerState ManagerState) (processStateSnapshot, error) {
	if err := managerState.Validate(); err != nil {
		return processStateSnapshot{}, fmt.Errorf("validate manager state: %w", err)
	}
	if err := ValidateCodespaceStateFiles(config.Node.StateDir); err != nil {
		return processStateSnapshot{}, fmt.Errorf("validate codespace state files: %w", err)
	}
	codespaceStateStore := NewCodespaceStateStore(config.Node.StateDir)
	initialOperations, err := codespaceStateStore.LoadActiveOperations()
	if err != nil {
		return processStateSnapshot{}, fmt.Errorf("load codespace active operations: %w", err)
	}
	initialRuntimeGenerations, err := codespaceStateStore.LoadRuntimeGenerations()
	if err != nil {
		return processStateSnapshot{}, fmt.Errorf("load codespace runtime generations: %w", err)
	}
	initialRuntimeTransitions, err := codespaceStateStore.LoadRuntimeTransitionPendings()
	if err != nil {
		return processStateSnapshot{}, fmt.Errorf("load codespace runtime transitions: %w", err)
	}
	initialCleanupPendings, err := codespaceStateStore.LoadCleanupPendings()
	if err != nil {
		return processStateSnapshot{}, fmt.Errorf("load codespace cleanup pendings: %w", err)
	}
	initialHealthStopPendings, err := codespaceStateStore.LoadHealthStopPendings()
	if err != nil {
		return processStateSnapshot{}, fmt.Errorf("load codespace health stop pendings: %w", err)
	}
	initialGatewayRoutes, err := codespaceStateStore.LoadGatewayRoutes()
	if err != nil {
		return processStateSnapshot{}, fmt.Errorf("load codespace gateway routes: %w", err)
	}
	gatewaySSHHostKey, err := loadOrCreateGatewaySSHHostKey(config.Node.StateDir)
	if err != nil {
		return processStateSnapshot{}, fmt.Errorf("load gateway ssh host key: %w", err)
	}
	return processStateSnapshot{
		managerState:              managerState,
		codespaceStateStore:       codespaceStateStore,
		initialOperations:         initialOperations,
		initialRuntimeGenerations: initialRuntimeGenerations,
		initialRuntimeTransitions: initialRuntimeTransitions,
		initialCleanupPendings:    initialCleanupPendings,
		initialHealthStopPendings: initialHealthStopPendings,
		initialGatewayRoutes:      initialGatewayRoutes,
		gatewaySSHHostKey:         gatewaySSHHostKey,
	}, nil
}

type processRuntime struct {
	agent               *manager.Agent
	processHealth       *processHealth
	gatewayServer       *http.Server
	gatewaySSHServer    *gatewaySSHServer
	registryCacheServer *http.Server
}

func newProcessRuntime(ctx context.Context, config Config, state processStateSnapshot, registryCache *registryCache, workerEnabled bool) (*processRuntime, error) {
	managerProvisioner, err := newProvisioner(config, state.managerState.ManagerID, registryCache)
	if err != nil {
		return nil, fmt.Errorf("create provisioner: %w", err)
	}
	gatewayBackend, ok := managerProvisioner.(gatewayWorkspaceBackend)
	if !ok {
		return nil, fmt.Errorf("provisioner does not support gateway workspace access")
	}

	sessionRegistry := newGatewaySessionRegistryFromConfig(config.Gateway)
	gatewayRoutes := newGatewayRouteStore()
	gatewayRoutes.SetTCPBackend(gatewayBackend)
	state.codespaceStateStore.SetSessionRegistry(sessionRegistry)
	gatewayRoutes.SetSessionRegistry(sessionRegistry)
	for _, route := range state.initialGatewayRoutes {
		if err := gatewayRoutes.Put(route); err != nil {
			return nil, fmt.Errorf("load gateway route %s/%s: %w", route.codespaceUUID, route.endpointID, err)
		}
	}
	gatewayAccess := newGatewayAccessControllerFromConfig(config.Gateway)
	gatewayBrowserAuth := newGatewayBrowserAuth()
	gatewayOrigin, err := newGatewayOriginPolicy(config.Gateway.HTTP.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("configure gateway origin: %w", err)
	}
	gatewayControlPlane := newGatewayControlPlane(
		managerServiceBaseURL(state.managerState.GiteaURL),
		state.managerState.ManagerID,
		state.managerState.ManagerSecret,
		&http.Client{Timeout: config.Node.HTTPTimeout.ToStdlib()},
	)
	var runtimeMetadataPublisher *runtimeMetadataPublisher
	managerServiceSettings := managerServiceSettingsStores{
		gatewayControlPlane,
		gatewayBrowserAuth,
	}
	var agent *manager.Agent
	if workerEnabled {
		runtimeMetadataPublisher = newRuntimeMetadataPublisher(state.codespaceStateStore, gatewayControlPlane, managerProvisioner, 0)
		runtimeMetadataPublisher.Run(ctx)
		managerServiceSettings = append(managerServiceSettings, runtimeMetadataPublisher)
		endpointApplier := newRuntimeEndpointApplier(state.codespaceStateStore, gatewayRoutes, runtimeMetadataPublisher)
		environments := make([]*codespacev1.EnvironmentTag, 0, len(config.Runtime.Environments))
		for _, environment := range config.Runtime.Environments {
			environments = append(environments, &codespacev1.EnvironmentTag{
				Tag:         environment.Tag,
				Description: environment.Description,
			})
		}
		sort.Slice(environments, func(i, j int) bool {
			return environments[i].GetTag() < environments[j].GetTag()
		})

		agent = manager.New(manager.AgentConfig{
			BaseURL:                      managerServiceBaseURL(state.managerState.GiteaURL),
			ManagerID:                    state.managerState.ManagerID,
			ManagerSecret:                state.managerState.ManagerSecret,
			Name:                         config.Node.Name,
			GatewayURL:                   config.Gateway.HTTP.PublicURL,
			GatewaySSHAddr:               config.Gateway.SSH.PublicAddr,
			GatewaySSHHostKeyAlgo:        state.gatewaySSHHostKey.algorithm,
			GatewaySSHHostKeySHA256:      state.gatewaySSHHostKey.fingerprintSHA256,
			GatewaySSHHostKeyUnix:        state.gatewaySSHHostKey.updatedUnix,
			Version:                      managerBuildVersion(),
			Environments:                 environments,
			PollInterval:                 config.Node.PollInterval.ToStdlib(),
			DeclareInterval:              config.Node.DeclareInterval.ToStdlib(),
			CapacityTotal:                config.Node.CapacityTotal,
			StartupWorkers:               config.Node.StartupWorkers,
			CleanupWorkers:               config.Node.CleanupWorkers,
			HTTPTimeout:                  config.Node.HTTPTimeout.ToStdlib(),
			RuntimeMetadataGeneration:    1,
			InventoryGeneration:          state.managerState.InventoryGeneration,
			InitialRuntimeGenerations:    state.initialRuntimeGenerations,
			InitialRuntimeTransitions:    state.initialRuntimeTransitions,
			InitialCleanupPendings:       state.initialCleanupPendings,
			InitialHealthStopPendings:    state.initialHealthStopPendings,
			InitialOperations:            state.initialOperations,
			OperationStateStore:          state.codespaceStateStore,
			InventoryStateStore:          NewManagerStateStore(config.Node.StateDir),
			RuntimeStateStore:            state.codespaceStateStore,
			CleanupStateStore:            state.codespaceStateStore,
			HealthStopStateStore:         state.codespaceStateStore,
			RuntimeEnvironmentStateStore: state.codespaceStateStore,
			RuntimeMetadataStateStore:    state.codespaceStateStore,
			StartupInputStateStore:       state.codespaceStateStore,
			RuntimeEndpointApplier:       endpointApplier,
			RuntimeHealthStateStore:      state.codespaceStateStore,
			RuntimeMetadataPublisher:     runtimeMetadataPublisher,
			SessionTracker:               sessionRegistry,
			AccessController:             gatewayRoutes,
			ManagerServiceSettings:       managerServiceSettings,
			GitSSHKeyType:                config.runtimeGitSSHKeyType(),
		}, &http.Client{Timeout: config.Node.HTTPTimeout.ToStdlib()}, managerProvisioner)
	}

	processHealth := newProcessHealth()
	gatewayServer := newGatewayHTTPServer(newGatewayHandlerWithOriginAndBrowserAuth(
		processHealth,
		sessionRegistry,
		gatewayAccess,
		gatewayControlPlane,
		gatewayOrigin,
		gatewayBrowserAuth,
		gatewayRoutes,
	))
	gatewaySSHServer, err := newGatewaySSHServer(state.gatewaySSHHostKey.signer, state.codespaceStateStore, gatewayBackend, gatewayControlPlane, sessionRegistry, gatewayAccess, config.Gateway)
	if err != nil {
		return nil, fmt.Errorf("create gateway ssh server: %w", err)
	}
	return &processRuntime{
		agent:               agent,
		processHealth:       processHealth,
		gatewayServer:       gatewayServer,
		gatewaySSHServer:    gatewaySSHServer,
		registryCacheServer: newRegistryCacheHTTPServer(registryCache),
	}, nil
}

type processListeners struct {
	GatewayHTTP   net.Listener
	GatewaySSH    net.Listener
	RegistryCache net.Listener
}

func openProcessListeners(config Config, registryCache *registryCache) (*processListeners, error) {
	listeners := &processListeners{}
	var err error
	defer func() {
		if err != nil {
			listeners.Close()
		}
	}()

	listeners.GatewayHTTP, err = net.Listen("tcp", config.Gateway.HTTP.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen gateway http %s: %w", config.Gateway.HTTP.Listen, err)
	}
	listeners.GatewaySSH, err = net.Listen("tcp", config.Gateway.SSH.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen gateway ssh %s: %w", config.Gateway.SSH.Listen, err)
	}
	listeners.RegistryCache, err = registryCache.OpenListener()
	if err != nil {
		return nil, err
	}
	return listeners, nil
}

func (l *processListeners) Close() {
	if l == nil {
		return
	}
	if l.GatewayHTTP != nil {
		_ = l.GatewayHTTP.Close()
	}
	if l.GatewaySSH != nil {
		_ = l.GatewaySSH.Close()
	}
	if l.RegistryCache != nil {
		_ = l.RegistryCache.Close()
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

func newProvisioner(config Config, managerID int64, registryCache *registryCache) (provisioner.Provisioner, error) {
	switch config.provisionerKind {
	case "dummy":
		return provisioner.NewDummy(), nil
	case "", "incus":
		remote, unixSocket, err := incusEndpoint(config.Runtime.Incus.Endpoint)
		if err != nil {
			return nil, err
		}
		var cacheOptions provisioner.RuntimeCacheOptionsFunc
		if registryCache != nil && registryCache.enabled {
			cacheOptions = registryCache.CacheOptions
		}
		return provisioner.NewIncus(provisioner.IncusConfig{
			ManagerID:           managerID,
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
			BuildCacheRegistry:  registryCacheBuildRegistry(registryCache),
			RegistryMirrors:     registryCacheMirrors(registryCache),
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

func newGatewayHandlerWithOriginAndBrowserAuth(
	health *processHealth,
	sessions *gatewaySessionRegistry,
	access *gatewayAccessController,
	controlPlane *gatewayControlPlane,
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
	controlPlane *gatewayControlPlane,
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
	controlPlane *gatewayControlPlane,
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
		externalHost:   gatewayExternalHost(request),
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
	controlPlane *gatewayControlPlane,
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
		externalHost:   gatewayExternalHost(request),
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

func proxyGatewayEndpoint(
	writer http.ResponseWriter,
	request *http.Request,
	routes *gatewayRouteStore,
	route gatewayEndpointRoute,
	upstreamPath string,
	proxyContext gatewayProxyRequestContext,
) {
	upstreamHost := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(route.upstreamPort)))
	target := &url.URL{Scheme: "http", Host: upstreamHost}
	proxy := &httputil.ReverseProxy{}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if routes == nil {
				return nil, fmt.Errorf("gateway route store is unavailable")
			}
			return routes.OpenEndpointTCP(ctx, route)
		},
	}
	defer transport.CloseIdleConnections()
	proxy.Transport = transport
	proxy.Rewrite = func(proxyRequest *httputil.ProxyRequest) {
		proxyRequest.Out.URL.Scheme = target.Scheme
		proxyRequest.Out.URL.Host = target.Host
		proxyRequest.Out.URL.Path = upstreamPath
		proxyRequest.Out.URL.RawPath = ""
		proxyRequest.Out.Host = target.Host
		prepareGatewayProxyRequest(proxyRequest.Out, proxyContext)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		normalizeGatewayProxyResponse(response.Header, gatewayProxyResponseContext{
			externalScheme: proxyContext.externalScheme,
			externalHost:   proxyContext.externalHost,
			upstreamHost:   upstreamHost,
		})
		return nil
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
		log.Printf("gateway proxy %s/%s: %v", route.codespaceUUID, route.endpointID, err)
		title := "Endpoint is unavailable"
		message := "Codespace Gateway could not connect to the runtime endpoint. The service may still be starting."
		if route.endpointID == runtimeendpoint.WorkspaceEndpointID {
			title = "Workspace is unavailable"
			message = "Codespace Gateway could not connect to the Web IDE. The development environment may still be starting."
		}
		writeGatewayError(writer, request, http.StatusBadGateway, title, message, "gateway upstream unavailable")
	}
	proxy.ServeHTTP(writer, request)
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

func gatewayExternalHost(request *http.Request) string {
	if request == nil {
		return ""
	}
	return request.Host
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

func writeGatewayErrorHTML(writer http.ResponseWriter, statusCode int, title, message, category string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	writer.WriteHeader(statusCode)
	_, _ = fmt.Fprintf(writer, gatewayErrorPageHTML,
		statusCode,
		html.EscapeString(http.StatusText(statusCode)),
		html.EscapeString(title),
		html.EscapeString(message),
		html.EscapeString(category),
	)
}

const gatewayErrorPageHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Codespace Gateway</title>
<style>
html, body {
  box-sizing: border-box;
  min-height: 100%%;
  margin: 0;
}
body {
  display: grid;
  place-items: center;
  padding: 32px;
  background: #0f1419;
  color: #dce3ea;
  font: 14px/1.5 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
main {
  width: min(560px, 100%%);
  border: 1px solid #27313b;
  border-radius: 8px;
  background: #151b22;
  box-shadow: 0 18px 60px rgba(0, 0, 0, .32);
}
header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 16px 18px;
  border-bottom: 1px solid #27313b;
}
.brand {
  font-weight: 600;
}
.status {
  color: #8ea0b2;
  font-size: 12px;
}
section {
  padding: 28px;
}
h1 {
  margin: 0 0 10px;
  font-size: 22px;
  line-height: 1.25;
}
p {
  margin: 0;
  color: #aab7c4;
}
.category {
  margin-top: 22px;
  padding: 10px 12px;
  border-radius: 6px;
  background: #0f1419;
  color: #91a4b7;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  overflow-wrap: anywhere;
}
</style>
</head>
<body>
<main>
<header>
<div class="brand">Codespace Gateway</div>
<div class="status">%d %s</div>
</header>
<section>
<h1>%s</h1>
<p>%s</p>
<div class="category">%s</div>
</section>
</main>
</body>
</html>
`

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		log.Printf("%s %s", request.Method, request.URL.Path)
		next.ServeHTTP(writer, request)
	})
}
