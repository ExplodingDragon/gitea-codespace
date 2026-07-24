// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
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

	config, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return RunWithConfig(output, config)
}

// RunWithConfig starts the Manager worker and the process health endpoint.
func RunWithConfig(output io.Writer, config Config) error {
	if output == nil {
		return fmt.Errorf("output is nil")
	}

	credentials, err := LoadManagerCredentials(config.Manager.StateDir)
	if err != nil {
		return fmt.Errorf("load manager credentials: %w", err)
	}
	stateLock, err := acquireStateDirLock(config.Manager.StateDir)
	if err != nil {
		return fmt.Errorf("acquire manager state dir lock: %w", err)
	}
	defer func() {
		if err := stateLock.Close(); err != nil {
			log.Printf("release manager state dir lock: %v", err)
		}
	}()
	rootState, err := LoadManagerRootState(config.Manager.StateDir, credentials)
	if err != nil {
		return fmt.Errorf("load manager root state: %w", err)
	}
	if err := ValidateCodespaceStateFiles(config.Manager.StateDir); err != nil {
		return fmt.Errorf("validate codespace state files: %w", err)
	}
	codespaceStateStore := NewCodespaceStateStore(config.Manager.StateDir)
	initialOperations, err := codespaceStateStore.LoadActiveOperations()
	if err != nil {
		return fmt.Errorf("load codespace active operations: %w", err)
	}
	initialRuntimeGenerations, err := codespaceStateStore.LoadRuntimeGenerations()
	if err != nil {
		return fmt.Errorf("load codespace runtime generations: %w", err)
	}
	initialRuntimeTransitions, err := codespaceStateStore.LoadRuntimeTransitionPendings()
	if err != nil {
		return fmt.Errorf("load codespace runtime transitions: %w", err)
	}
	initialCleanupPendings, err := codespaceStateStore.LoadCleanupPendings()
	if err != nil {
		return fmt.Errorf("load codespace cleanup pendings: %w", err)
	}
	initialGatewayRoutes, err := codespaceStateStore.LoadGatewayRoutes()
	if err != nil {
		return fmt.Errorf("load codespace gateway routes: %w", err)
	}
	initialRuntimeMetadataUUIDs, err := codespaceStateStore.LoadRuntimeMetadataCodespaceUUIDs()
	if err != nil {
		return fmt.Errorf("load codespace runtime metadata snapshots: %w", err)
	}
	managerProvisioner, err := newProvisioner(config, credentials.ManagerID)
	if err != nil {
		return fmt.Errorf("create provisioner: %w", err)
	}

	listeners, err := openProcessListeners(config)
	if err != nil {
		return err
	}
	defer listeners.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancelProcess := context.WithCancel(ctx)
	defer cancelProcess()
	sessionRegistry := newGatewaySessionRegistry()
	gatewayRoutes := newGatewayRouteStore()
	gatewayRoutes.SetSessionRegistry(sessionRegistry)
	for _, route := range initialGatewayRoutes {
		if err := gatewayRoutes.Put(route); err != nil {
			return fmt.Errorf("load gateway route %s/%s: %w", route.codespaceUUID, route.endpointID, err)
		}
	}
	gatewayAccess := newGatewayAccessControllerFromConfig(config.Gateway)
	gatewayBrowserAuth := newGatewayBrowserAuth()
	gatewayOrigin, err := newGatewayOriginPolicy(config.Manager.GatewayURL)
	if err != nil {
		return fmt.Errorf("configure gateway origin: %w", err)
	}
	gatewayControlPlane := newGatewayControlPlane(
		strings.TrimRight(config.Gitea.URL, "/"),
		credentials.ManagerID,
		credentials.ManagerSecret,
		&http.Client{Timeout: config.Manager.HTTPTimeout.ToStdlib()},
	)
	runtimeMetadataPublisher := newRuntimeMetadataPublisher(codespaceStateStore, gatewayControlPlane, 0)
	runtimeMetadataPublisher.Run(ctx, initialRuntimeMetadataUUIDs)
	managerServiceSettings := managerServiceSettingsStores{
		gatewayBrowserAuth,
		runtimeMetadataPublisher,
	}

	agent := manager.New(manager.AgentConfig{
		BaseURL:                   strings.TrimRight(config.Gitea.URL, "/"),
		ManagerID:                 credentials.ManagerID,
		ManagerSecret:             credentials.ManagerSecret,
		Name:                      config.Manager.Name,
		GatewayURL:                config.Manager.GatewayURL,
		GatewaySSHAddr:            config.Manager.GatewaySSHAddr,
		GatewaySSHHostKeyAlgo:     config.Manager.GatewaySSHHostKeyAlgorithm,
		GatewaySSHHostKeySHA256:   config.Manager.GatewaySSHHostKeyFingerprintSHA256,
		GatewaySSHHostKeyUnix:     config.Manager.GatewaySSHHostKeyUpdatedUnix,
		Version:                   config.Manager.Version,
		Tags:                      append([]string(nil), config.Manager.Tags...),
		PollInterval:              config.Manager.PollInterval.ToStdlib(),
		DeclareInterval:           config.Manager.DeclareInterval.ToStdlib(),
		CapacityTotal:             config.Manager.CapacityTotal,
		CapacityAvailable:         config.Manager.CapacityAvailable,
		CleanupCapacityAvailable:  config.Manager.CleanupCapacityAvailable,
		MaxOperations:             config.Manager.MaxOperations,
		HTTPTimeout:               config.Manager.HTTPTimeout.ToStdlib(),
		RuntimeMetadataGeneration: 1,
		InventoryGeneration:       rootState.InventoryGeneration,
		InitialRuntimeGenerations: initialRuntimeGenerations,
		InitialRuntimeTransitions: initialRuntimeTransitions,
		InitialCleanupPendings:    initialCleanupPendings,
		InitialOperations:         initialOperations,
		OperationStateStore:       codespaceStateStore,
		InventoryStateStore:       NewManagerRootStateStore(config.Manager.StateDir, credentials.ManagerID),
		RuntimeStateStore:         codespaceStateStore,
		CleanupStateStore:         codespaceStateStore,
		RuntimeCredentialStore:    codespaceStateStore,
		RuntimeMetadataStateStore: codespaceStateStore,
		RuntimeMetadataPublisher:  runtimeMetadataPublisher,
		RuntimeAPIBaseURL:         config.Server.RuntimeAPIURL,
		SessionTracker:            sessionRegistry,
		AccessController:          gatewayRoutes,
		ManagerServiceSettings:    managerServiceSettings,
	}, &http.Client{Timeout: config.Manager.HTTPTimeout.ToStdlib()}, managerProvisioner)

	processHealth := newProcessHealth()
	runtimeAPIServer := &http.Server{
		Handler:           newRuntimeAPIHandler(processHealth, newRuntimeAPIService(codespaceStateStore, gatewayRoutes, gatewayControlPlane, runtimeSourceResolverFor(managerProvisioner), runtimeMetadataPublisher)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	gatewayServer := newGatewayHTTPServer(newGatewayHandlerWithOriginAndBrowserAuth(
		processHealth,
		sessionRegistry,
		gatewayAccess,
		gatewayControlPlane,
		gatewayOrigin,
		gatewayBrowserAuth,
		gatewayRoutes,
	))

	errorChannel := make(chan error, 4)
	go serveHTTP(ctx, errorChannel, "runtime api", runtimeAPIServer, listeners.RuntimeAPI)
	go serveHTTP(ctx, errorChannel, "gateway http", gatewayServer, listeners.GatewayHTTP)
	go serveSSH(ctx, errorChannel, listeners.GatewaySSH)
	fmt.Fprintf(output, "codespace runtime api listening on %s\n", listeners.RuntimeAPI.Addr())
	fmt.Fprintf(output, "codespace gateway http listening on %s\n", listeners.GatewayHTTP.Addr())
	fmt.Fprintf(output, "codespace gateway ssh listening on %s\n", listeners.GatewaySSH.Addr())

	go func() {
		if err := agent.Run(ctx); err != nil {
			errorChannel <- fmt.Errorf("manager: %w", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errorChannel:
		runErr = err
		cancelProcess()
	}
	processHealth.Fail()

	shutdownContext, cancel := context.WithTimeout(context.Background(), config.Server.ShutdownTimeout.ToStdlib())
	defer cancel()
	if err := runtimeAPIServer.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shutdown runtime api server: %w", err)
	}
	if err := gatewayServer.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shutdown gateway server: %w", err)
	}
	listeners.Close()
	return runErr
}

type processListeners struct {
	RuntimeAPI  net.Listener
	GatewayHTTP net.Listener
	GatewaySSH  net.Listener
}

func openProcessListeners(config Config) (*processListeners, error) {
	runtimeAPI, err := net.Listen("tcp", config.Server.RuntimeAPIListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen runtime api %s: %w", config.Server.RuntimeAPIListenAddr, err)
	}
	listeners := &processListeners{RuntimeAPI: runtimeAPI}
	defer func() {
		if err != nil {
			listeners.Close()
		}
	}()

	listeners.GatewayHTTP, err = net.Listen("tcp", config.Server.GatewayListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen gateway http %s: %w", config.Server.GatewayListenAddr, err)
	}
	listeners.GatewaySSH, err = net.Listen("tcp", config.Server.GatewaySSHListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen gateway ssh %s: %w", config.Server.GatewaySSHListenAddr, err)
	}
	return listeners, nil
}

func (l *processListeners) Close() {
	if l == nil {
		return
	}
	if l.RuntimeAPI != nil {
		_ = l.RuntimeAPI.Close()
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

func serveSSH(ctx context.Context, errorChannel chan<- error, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) && ctx.Err() != nil {
				return
			}
			errorChannel <- fmt.Errorf("gateway ssh listener: %w", err)
			return
		}
		_ = conn.Close()
	}
}

func newProvisioner(config Config, managerID int64) (provisioner.Provisioner, error) {
	switch strings.ToLower(strings.TrimSpace(config.Provisioner.Kind)) {
	case "dummy":
		return provisioner.NewDummy(), nil
	case "incus":
		return provisioner.NewIncus(provisioner.IncusConfig{
			ManagerID:              managerID,
			Project:                config.Provisioner.Incus.Project,
			Remote:                 config.Provisioner.Incus.Remote,
			UnixSocket:             config.Provisioner.Incus.UnixSocket,
			CodespaceRoot:          config.Provisioner.CodespaceRoot,
			CommunicationInterface: config.Provisioner.Incus.CommunicationInterface,
			Bootstrap: provisioner.BootstrapConfig{
				Shell:   config.Provisioner.Bootstrap.Shell,
				HomeDir: config.Provisioner.Bootstrap.HomeDir,
				User:    config.Provisioner.Bootstrap.User,
				Group:   config.Provisioner.Bootstrap.Group,
			},
		})
	default:
		return nil, fmt.Errorf("unknown provisioner kind %q", config.Provisioner.Kind)
	}
}

func runtimeSourceResolverFor(managerProvisioner provisioner.Provisioner) runtimeSourceResolver {
	resolver, ok := managerProvisioner.(runtimeSourceResolver)
	if !ok {
		return nil
	}
	return resolver
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

func newRuntimeAPIHandler(health *processHealth, services ...*runtimeAPIService) http.Handler {
	var runtimeAPI *runtimeAPIService
	if len(services) > 0 {
		runtimeAPI = services[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(writer, request)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"name":   "gitea-codespace",
			"status": "ok",
		})
	})
	mux.HandleFunc("/api/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		health.writeHealthz(writer)
	})
	if runtimeAPI != nil {
		mux.HandleFunc(runtimeGitSSHKeyAPIPath, runtimeAPI.handleGitSSHKey)
		mux.HandleFunc(runtimeEndpointAPIPrefix, runtimeAPI.handleEndpoint)
	}
	return loggingMiddleware(mux)
}

func newGatewayHandler(
	health *processHealth,
	sessions *gatewaySessionRegistry,
	access *gatewayAccessController,
	controlPlane *gatewayControlPlane,
) http.Handler {
	return newGatewayHandlerWithOrigin(health, sessions, access, controlPlane, gatewayOriginPolicy{})
}

func newGatewayHandlerWithOrigin(
	health *processHealth,
	sessions *gatewaySessionRegistry,
	access *gatewayAccessController,
	controlPlane *gatewayControlPlane,
	originPolicy gatewayOriginPolicy,
) http.Handler {
	return newGatewayHandlerWithOriginAndBrowserAuth(health, sessions, access, controlPlane, originPolicy, nil)
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
		if request.URL.Path != "/" {
			http.NotFound(writer, request)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"name":   "gitea-codespace-gateway",
			"status": "ok",
		})
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
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if originPolicy.domain != "" && request.URL.Path != "/.gitea-codespace/open" {
		http.NotFound(writer, request)
		return
	}
	if sessions == nil || access == nil || controlPlane == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway is not ready"})
		return
	}
	hostBinding, hasHostBinding := originPolicy.bindingForRequest(request)
	if originPolicy.domain != "" && !hasHostBinding {
		http.NotFound(writer, request)
		return
	}
	code, ok := gatewayOpenCode(request)
	if !ok {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": "invalid open code request"})
		return
	}
	reservation, limitStatus := access.reserveRequest()
	if limitStatus != 0 {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway capacity unavailable"})
		return
	}
	defer reservation.Release()

	decision, err := controlPlane.validateOpenToken(request.Context(), code)
	if err != nil {
		log.Printf("validate open token: %v", err)
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway authorization unavailable"})
		return
	}
	if !decision.allowed {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": decision.deniedCategory})
		return
	}
	if hasHostBinding &&
		(hostBinding.codespaceUUID != decision.binding.codespaceUUID ||
			hostBinding.endpointID != decision.binding.endpointID) {
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": "gateway host binding mismatch"})
		return
	}
	sessionID, err := sessions.Create(decision.binding, time.Now())
	if err != nil {
		log.Printf("create gateway session: %v", err)
		clearGatewayReturnToIfPresent(writer, request, originPolicy)
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway session unavailable"})
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
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway is not ready"})
		return
	}
	codespaceUUID, endpointID, upstreamPath, ok := resolveGatewayWorkspaceBinding(request, originPolicy)
	if !ok {
		http.NotFound(writer, request)
		return
	}
	if rejectGatewayServiceWorkerRequest(writer, request) {
		return
	}
	if !isGatewayAuthenticatedSourceAllowed(request, originPolicy) {
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": "gateway source is not allowed"})
		return
	}
	sessionID, ok := gatewaySessionIDFromRequest(request, originPolicy)
	if !ok {
		if handleGatewayAuthenticationRequired(writer, request, codespaceUUID, endpointID, originPolicy, browserAuth) {
			return
		}
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": "gateway session is required"})
		return
	}
	session, ok := sessions.Authenticate(sessionID, codespaceUUID, endpointID, time.Now())
	if !ok {
		if handleGatewayAuthenticationRequired(writer, request, codespaceUUID, endpointID, originPolicy, browserAuth) {
			return
		}
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": "gateway session is invalid"})
		return
	}
	reservation, limitStatus := access.reserveSessionRequest(session.id)
	if limitStatus != 0 {
		if limitStatus == http.StatusTooManyRequests {
			writeJSON(writer, http.StatusTooManyRequests, map[string]any{"error": "gateway session request limit reached"})
			return
		}
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway capacity unavailable"})
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
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway authorization capacity unavailable"})
		return
	}
	if err != nil {
		log.Printf("revalidate gateway session: %v", err)
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway authorization unavailable"})
		return
	}
	if !decision.allowed {
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": decision.deniedCategory})
		return
	}
	end := sessions.Begin(session.codespaceUUID)
	defer end()

	if routes == nil {
		writeJSON(writer, http.StatusOK, map[string]any{
			"codespace_uuid": session.codespaceUUID,
			"endpoint_id":    session.endpointID,
			"status":         "authorized",
		})
		return
	}
	route, routeRequest, releaseRoute, ok := routes.BeginProxy(request, session.codespaceUUID, session.endpointID)
	if !ok {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway route unavailable"})
		return
	}
	defer releaseRoute()
	proxyRequest, cancelProxyRevalidation := withGatewayProxyRevalidation(
		routeRequest,
		access.config.streamRevalidateInterval,
		"revalidate gateway endpoint session",
		func(ctx context.Context) (gatewayAccessDecision, error) {
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
		},
	)
	defer cancelProxyRevalidation()
	proxyGatewayEndpoint(writer, proxyRequest, route, upstreamPath, gatewayProxyRequestContext{
		codespaceUUID:  session.codespaceUUID,
		endpointID:     session.endpointID,
		access:         "authenticated",
		userID:         session.userID,
		externalScheme: gatewayExternalScheme(request, originPolicy),
		externalHost:   gatewayExternalHost(request),
	})
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
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway is not ready"})
		return
	}
	codespaceUUID, endpointID, upstreamPath, ok := resolveGatewayPublicEndpointBinding(request, originPolicy)
	if !ok {
		http.NotFound(writer, request)
		return
	}
	if rejectGatewayServiceWorkerRequest(writer, request) {
		return
	}
	clearGatewayReservedCookies(writer)
	reservation, limitStatus := access.reservePublic(codespaceUUID, endpointID, gatewayPeerIP(request))
	if limitStatus != 0 {
		if limitStatus == http.StatusTooManyRequests {
			writeJSON(writer, http.StatusTooManyRequests, map[string]any{"error": "gateway public connection limit reached"})
			return
		}
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway capacity unavailable"})
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
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway authorization capacity unavailable"})
		return
	}
	if err != nil {
		log.Printf("validate public endpoint: %v", err)
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway authorization unavailable"})
		return
	}
	if !decision.allowed {
		http.NotFound(writer, request)
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
		http.NotFound(writer, request)
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
	proxyGatewayEndpoint(writer, proxyRequest, route, upstreamPath, gatewayProxyRequestContext{
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
		return parts[0], "workspace", "/", true
	}
	if len(parts) >= 3 && parts[1] == "e" && parts[2] != "" && parts[2] != "workspace" {
		return parts[0], parts[2], gatewayProxyPathFromParts(parts[3:]), true
	}
	return parts[0], "workspace", gatewayProxyPathFromParts(parts[1:]), true
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
	if !pathOK && request.URL.Path != "/w/" {
		return "", "", "", false
	}
	if !pathOK {
		upstreamPath = "/"
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
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" || parts[1] == "workspace" {
		return "", "", "", false
	}
	return parts[0], parts[1], gatewayProxyPathFromParts(parts[2:]), true
}

func resolveGatewayPublicEndpointBinding(request *http.Request, originPolicy gatewayOriginPolicy) (string, string, string, bool) {
	if originPolicy.domain == "" {
		return parseGatewayPublicEndpointPath(request.URL.Path)
	}
	hostBinding, ok := originPolicy.bindingForRequest(request)
	if !ok || hostBinding.endpointID == "workspace" {
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
	route gatewayEndpointRoute,
	upstreamPath string,
	proxyContext gatewayProxyRequestContext,
) {
	target := &url.URL{Scheme: route.upstreamScheme, Host: route.upstreamHost}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(upstream *http.Request) {
		upstream.URL.Scheme = target.Scheme
		upstream.URL.Host = target.Host
		upstream.URL.Path = upstreamPath
		upstream.URL.RawPath = ""
		upstream.Host = target.Host
		prepareGatewayProxyRequest(upstream, proxyContext)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		normalizeGatewayProxyResponse(response.Header, gatewayProxyResponseContext{
			externalScheme: proxyContext.externalScheme,
			externalHost:   proxyContext.externalHost,
			upstreamScheme: route.upstreamScheme,
			upstreamHost:   route.upstreamHost,
		})
		return nil
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
		log.Printf("gateway proxy %s/%s: %v", route.codespaceUUID, route.endpointID, err)
		writeJSON(writer, http.StatusBadGateway, map[string]any{"error": "gateway upstream unavailable"})
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
	writeJSON(writer, http.StatusForbidden, map[string]any{"error": "service worker is not allowed"})
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
	if endpointID == "" || endpointID == "workspace" {
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

func gatewaySessionIDFromRequest(request *http.Request, originPolicy gatewayOriginPolicy) (string, bool) {
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
	if len(values) != 1 {
		return "", false
	}
	for value := range values {
		return value, true
	}
	return "", false
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

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		log.Printf("%s %s", request.Method, request.URL.Path)
		next.ServeHTTP(writer, request)
	})
}
