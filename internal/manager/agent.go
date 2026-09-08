// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/controlplane"
	"gitea.dev/codespace/internal/provisioner"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	initialReadMaxBytes = 64 * 1024

	gitSSHKeyTypeEd25519 = "ed25519"
	gitSSHKeyTypeRSA4096 = "rsa-4096"

	defaultInventoryInterval        = time.Minute
	maxInventoryInstances           = 10000
	runtimeHealthFailuresBeforeStop = 3

	// RuntimeBootStagePrepareRuntime means the Manager has started preparing the runtime.
	RuntimeBootStagePrepareRuntime = "prepare-runtime"
	// RuntimeBootStageBootstrapSystem means the Manager is preparing system credentials.
	RuntimeBootStageBootstrapSystem = "initialize-system"
	// RuntimeBootStagePrepareWorkspace means the workspace path is known for this startup.
	RuntimeBootStagePrepareWorkspace = "prepare-workspace"
	// RuntimeBootStageStartEnvironment means the workspace environment is starting.
	RuntimeBootStageStartEnvironment = "start-environment"
	// RuntimeBootStagePublishReady means the runtime is validated and ready metadata is being published.
	RuntimeBootStagePublishReady = "publish-ready"
	// RuntimeBootStageReady means the runtime is ready for user entry.
	RuntimeBootStageReady = "ready"
)

var (
	runtimeSecretNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

var runtimeBootStageRanks = map[string]int{
	RuntimeBootStagePrepareRuntime:   0,
	RuntimeBootStageBootstrapSystem:  1,
	RuntimeBootStagePrepareWorkspace: 2,
	RuntimeBootStageStartEnvironment: 3,
	RuntimeBootStagePublishReady:     4,
	RuntimeBootStageReady:            5,
}

// IsRuntimeBootStage reports whether stage is defined by the Runtime Metadata protocol.
func IsRuntimeBootStage(stage string) bool {
	_, ok := runtimeBootStageRanks[stage]
	return ok
}

// RuntimeBootStageProto converts the local boot stage name to the control-plane enum.
func RuntimeBootStageProto(stage string) (codespacev1.RuntimeBootStage, bool) {
	switch stage {
	case RuntimeBootStagePrepareRuntime:
		return codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_RUNTIME, true
	case RuntimeBootStageBootstrapSystem:
		return codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_INITIALIZE_SYSTEM, true
	case RuntimeBootStagePrepareWorkspace:
		return codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_WORKSPACE, true
	case RuntimeBootStageStartEnvironment:
		return codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_START_ENVIRONMENT, true
	case RuntimeBootStagePublishReady:
		return codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PUBLISH_READY, true
	case RuntimeBootStageReady:
		return codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY, true
	default:
		return codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_UNSPECIFIED, false
	}
}

// RuntimeMetadataProto builds the typed Runtime Metadata request payload.
func RuntimeMetadataProto(snapshot RuntimeMetadataSnapshot, endpoints []*codespacev1.RuntimeEndpoint) (*codespacev1.RuntimeMetadata, error) {
	stage, ok := RuntimeBootStageProto(snapshot.Boot.Stage)
	if !ok {
		return nil, fmt.Errorf("boot stage is invalid")
	}
	if endpoints == nil {
		endpoints = []*codespacev1.RuntimeEndpoint{}
	}
	return &codespacev1.RuntimeMetadata{
		Endpoints: endpoints,
		Boot: &codespacev1.RuntimeBoot{
			OperationRversion: snapshot.Boot.OperationRVersion,
			Stage:             stage,
			StartedUnix:       snapshot.Boot.StartedUnix,
			LastUpdateUnix:    snapshot.Boot.LastUpdateUnix,
		},
		ResourceUsage: runtimeResourceUsageProto(snapshot.ResourceUsage),
	}, nil
}

func runtimeResourceUsageProto(usage provisioner.RuntimeResourceUsage) *codespacev1.RuntimeResourceUsage {
	return &codespacev1.RuntimeResourceUsage{
		Cpu: &codespacev1.RuntimeCPUUsage{
			UsedMillicores:  usage.CPUUsedMillicores,
			LimitMillicores: usage.CPULimitMillicores,
		},
		Memory: &codespacev1.RuntimeMemoryUsage{
			UsedBytes:  usage.MemoryUsedBytes,
			LimitBytes: usage.MemoryLimitBytes,
		},
		Disk: &codespacev1.RuntimeDiskUsage{
			UsedBytes:  usage.DiskUsedBytes,
			LimitBytes: usage.DiskLimitBytes,
		},
		ObservedUnix: usage.ObservedUnix,
	}
}

// AgentConfig configures the Manager worker.
type AgentConfig struct {
	// ExecutionContext remains live during normal shutdown, but ends on leadership loss.
	ExecutionContext             context.Context
	BaseURL                      string
	ManagerID                    int64
	ManagerSecret                string
	Name                         string
	GatewayURL                   string
	GatewaySSHAddr               string
	GatewaySSHHostKeyAlgo        string
	GatewaySSHHostKeySHA256      string
	GatewaySSHHostKeyUnix        int64
	Version                      string
	Environments                 []*codespacev1.EnvironmentTag
	PollInterval                 time.Duration
	DeclareInterval              time.Duration
	CapacityTotal                int32
	StartupWorkers               int32
	CleanupWorkers               int32
	CapacitySiteID               int64
	CapacityCoordinator          CapacityCoordinator
	HTTPTimeout                  time.Duration
	ShutdownTimeout              time.Duration
	RuntimeMetadataGeneration    int64
	InventoryGeneration          int64
	InitialRuntimeGenerations    map[string]int64
	InitialRuntimeTransitions    []RuntimeTransitionSnapshot
	InitialCleanupPendings       []string
	InitialHealthStopPendings    []HealthStopSnapshot
	InitialOperations            []OperationSnapshot
	OperationStateStore          OperationStateStore
	InventoryStateStore          InventoryStateStore
	RuntimeStateStore            RuntimeStateStore
	CleanupStateStore            CleanupStateStore
	HealthStopStateStore         HealthStopStateStore
	RuntimeEnvironmentStateStore RuntimeEnvironmentStateStore
	RuntimeMetadataStateStore    RuntimeMetadataStateStore
	StartupInputStateStore       StartupInputStateStore
	RuntimeEndpointApplier       RuntimeEndpointApplier
	RuntimeHealthStateStore      RuntimeHealthStateStore
	RuntimeMetadataPublisher     RuntimeMetadataPublisher
	RuntimeIdentityStore         RuntimeIdentityStore
	SessionTracker               SessionTracker
	AccessController             AccessController
	ManagerServiceSettings       ManagerServiceSettingsStore
	GitSSHKeyType                string
}

// ManagerServiceSettings contains the current server-selected ManagerService values.
type ManagerServiceSettings struct {
	HeartbeatInterval              time.Duration
	RuntimeMetadataRefreshInterval time.Duration
	ControlPlaneMaxMessageSize     int64
	GiteaWebURL                    string
}

// ManagerServiceSettingsStore receives validated ManagerService settings.
type ManagerServiceSettingsStore interface {
	SaveManagerServiceSettings(settings ManagerServiceSettings) error
}

// SessionTracker reports authenticated live sessions by Codespace.
type SessionTracker interface {
	LiveSessions(codespaceUUID string) int
}

// AccessController closes local user traffic for one Codespace.
type AccessController interface {
	CloseCodespaceAccess(codespaceUUID string)
}

// OperationSnapshot stores one complete active operation context.
type OperationSnapshot struct {
	Payload     *codespacev1.OperationPayload
	WorkerStage OperationWorkerStage
}

// OperationWorkerStage stores the local worker stage for one active operation.
type OperationWorkerStage string

const (
	// OperationWorkerStageActive means the operation has a current local lease and may run.
	OperationWorkerStageActive OperationWorkerStage = "active"
	// OperationWorkerStageLeasePaused means the operation context is retained but local execution is paused.
	OperationWorkerStageLeasePaused OperationWorkerStage = "lease_paused"
	// OperationWorkerStageRecoveryBlocked retains an interrupted operation whose
	// remote effects are unknown. It must be cleared by control-plane reconciliation.
	OperationWorkerStageRecoveryBlocked OperationWorkerStage = "recovery_blocked"
)

// OperationStateStore persists operation contexts that must survive process restart.
type OperationStateStore interface {
	SaveActiveOperation(snapshot OperationSnapshot) error
	DeleteActiveOperation(codespaceUUID string, operationRVersion int64) error
}

// InventoryStateStore persists Manager-wide inventory state.
type InventoryStateStore interface {
	SaveInventoryGeneration(generation int64) error
}

// RuntimeTransitionSnapshot stores one pending Manager-initiated runtime state report.
type RuntimeTransitionSnapshot struct {
	CodespaceUUID             string
	TargetState               codespacev1.RuntimeState
	RuntimeGeneration         int64
	ObservedOperationRVersion int64
}

// RuntimeStateStore persists per-Codespace runtime state owned by the Manager.
type RuntimeStateStore interface {
	SaveRuntimeTransitionPending(snapshot RuntimeTransitionSnapshot) error
	ClearRuntimeTransitionPending(codespaceUUID string, runtimeGeneration int64) error
}

// CleanupStateStore persists per-Codespace cleanup state owned by the Manager.
type CleanupStateStore interface {
	SaveCleanupPending(codespaceUUID string) error
	ClearCodespaceState(codespaceUUID string) error
}

// HealthStopSnapshot stores one pending health-driven runtime stop.
type HealthStopSnapshot struct {
	CodespaceUUID             string
	ObservedOperationRVersion int64
}

// HealthStopStateStore persists health-driven stop intent before stopping runtime resources.
type HealthStopStateStore interface {
	SaveHealthStopPending(snapshot HealthStopSnapshot) error
}

// RuntimeEnvironmentStateStore persists the outer identity and complete Dev Container target.
type RuntimeEnvironmentStateStore interface {
	SaveRuntimeEnvironment(codespaceUUID string, environment provisioner.RuntimeEnvironment) error
	LoadRuntimeEnvironment(codespaceUUID string) (provisioner.RuntimeEnvironment, bool, error)
}

// StartupInput stores create-time inputs owned by the Manager after the operation is claimed.
type StartupInput struct {
	CodespaceUUID   string
	RepoFullName    string
	Username        string
	GitUserEmail    string
	RuntimeUserName string
	EnvironmentTag  string
	DevContainer    provisioner.DevContainerConfiguration
}

// StartupInputStateStore persists create-time startup inputs for resume.
type StartupInputStateStore interface {
	SaveStartupInput(input StartupInput) error
	LoadStartupInput(codespaceUUID string) (StartupInput, bool, error)
}

// RuntimeMetadataSnapshot stores the current complete runtime metadata base owned by a Codespace.
type RuntimeMetadataSnapshot struct {
	CodespaceUUID      string
	MetadataGeneration int64
	InstanceName       string
	Workdir            string
	Boot               RuntimeMetadataBoot
	ResourceUsage      provisioner.RuntimeResourceUsage
}

// RuntimeMetadataBoot stores the boot stage accepted for the current runtime.
type RuntimeMetadataBoot struct {
	OperationRVersion int64
	Stage             string
	StartedUnix       int64
	LastUpdateUnix    int64
}

// RuntimeMetadataStateStore persists runtime metadata snapshots for Endpoint updates.
type RuntimeMetadataStateStore interface {
	SaveRuntimeMetadataSnapshot(snapshot RuntimeMetadataSnapshot) error
	ClearRuntimeMetadata(codespaceUUID string) error
}

// RuntimeEndpointRoute stores one trusted runtime HTTP/WebSocket route.
type RuntimeEndpointRoute struct {
	CodespaceUUID string
	EndpointID    string
	Label         string
	InstanceName  string
	UpstreamPort  uint32
	Public        bool
}

// RuntimeEndpointApplier applies a complete runtime endpoint route set.
type RuntimeEndpointApplier interface {
	ApplyRuntimeEndpointRoutes(ctx context.Context, codespaceUUID string, routes []RuntimeEndpointRoute) error
}

// RuntimeHealthStateStore loads ready runtime metadata used by health checks.
type RuntimeHealthStateStore interface {
	LoadRuntimeMetadataSnapshot(codespaceUUID string) (RuntimeMetadataSnapshot, bool, error)
}

// RuntimeMetadataPublisher publishes the current complete metadata snapshot.
type RuntimeMetadataPublisher interface {
	ActivateRuntimeMetadata(codespaceUUID string) (bool, error)
	NotifyRuntimeMetadata(codespaceUUID string)
	PublishRuntimeMetadata(ctx context.Context, codespaceUUID string) error
	DeactivateRuntimeMetadata(ctx context.Context, codespaceUUID string)
}

// RuntimeIdentityStore persists the control-plane identity bound to a local runtime.
type RuntimeIdentityStore interface {
	SaveRuntimeIdentity(context.Context, string, int64, int64, string) error
	DeleteRuntimeIdentity(context.Context, string) error
}

type workspaceGitChecker interface {
	CheckWorkspaceGit(ctx context.Context, instanceName string, workdir string) (provisioner.WorkspaceGitStatus, error)
}

type workspaceAccessChecker interface {
	CheckWorkspaceAccess(ctx context.Context, instanceName string, workdir string) error
}

type runtimeDevelopmentEnvironmentChecker interface {
	CheckDevContainer(ctx context.Context, instanceName string) error
	CheckWorkspaceIDE(ctx context.Context, instanceName string, port uint32) error
}

type operationContext struct {
	recoveryBlocked   bool
	operationRVersion int64
	payload           *codespacev1.OperationPayload
	running           bool
	cancel            context.CancelFunc
	leaseTimer        *time.Timer
}

type finalizeOutcome int

const (
	finalizeOutcomeAccepted finalizeOutcome = iota
	finalizeOutcomeResourceAbsent
)

type idleStopOutcome int

const (
	idleStopOutcomePending idleStopOutcome = iota
	idleStopOutcomeObservationChanged
	idleStopOutcomeNotApplicable
)

type idleStopResult struct {
	outcome           idleStopOutcome
	operationRVersion int64
	runtimeSettings   *codespacev1.EffectiveCodespaceRuntimeSettings
	notApplicable     codespacev1.IdleStopNotApplicableReason
}

type autoStopState struct {
	settings        *codespacev1.EffectiveCodespaceRuntimeSettings
	runtimeState    codespacev1.RuntimeState
	metadataReady   bool
	idleStarted     time.Time
	requestInFlight bool
	retryAfter      time.Time
	pendingVersion  int64
}

type autoStopRequest struct {
	codespaceUUID string
	settings      *codespacev1.EffectiveCodespaceRuntimeSettings
}

// Agent runs one Codespace Manager against the Gitea ManagerService.
type Agent struct {
	config               AgentConfig
	baseURL              string
	httpClient           *http.Client
	clientMu             sync.RWMutex
	client               codespacev1connect.ManagerServiceClient
	serviceSettings      ManagerServiceSettings
	provisioner          provisioner.Provisioner
	metadataGeneration   int64
	metadataMu           sync.Mutex
	inventoryGeneration  int64
	inventoryMu          sync.Mutex
	runtimeMu            sync.Mutex
	runtimeGenerations   map[string]int64
	runtimeTransitions   map[string]RuntimeTransitionSnapshot
	cleanupPendings      map[string]struct{}
	healthStopPendings   map[string]HealthStopSnapshot
	activeMu             sync.Mutex
	activeOperations     map[string]*operationContext
	fetchReservedStartup int32
	fetchReservedCleanup int32
	stateStore           OperationStateStore
	inventoryStore       InventoryStateStore
	runtimeStateStore    RuntimeStateStore
	cleanupStateStore    CleanupStateStore
	healthStopStateStore HealthStopStateStore
	runtimeEnvStateStore RuntimeEnvironmentStateStore
	metadataStateStore   RuntimeMetadataStateStore
	startupInputStore    StartupInputStateStore
	endpointApplier      RuntimeEndpointApplier
	runtimeHealthStore   RuntimeHealthStateStore
	metadataPublisher    RuntimeMetadataPublisher
	runtimeIdentityStore RuntimeIdentityStore
	sessionTracker       SessionTracker
	accessController     AccessController
	settingsStore        ManagerServiceSettingsStore
	gitSSHKeyType        string
	autoStopMu           sync.Mutex
	autoStops            map[string]*autoStopState
	healthFailures       map[string]int
	healthCandidates     map[string]struct{}
	criticalErrors       chan error
	operationWorkers     sync.WaitGroup
	shutdownContext      context.Context
	capacityCoordinator  CapacityCoordinator
}

// New creates one Manager worker.
func New(config AgentConfig, httpClient *http.Client, provisioner provisioner.Provisioner) *Agent {
	client := controlplane.NewManagerServiceClient(httpClient, config.BaseURL, config.ManagerID, config.ManagerSecret, initialReadMaxBytes)
	metadataGeneration := config.RuntimeMetadataGeneration
	if metadataGeneration <= 0 {
		metadataGeneration = 1
	}
	startupInputStore := config.StartupInputStateStore
	gitSSHKeyType := normalizeRuntimeGitSSHKeyType(config.GitSSHKeyType)
	agent := &Agent{
		config:               config,
		baseURL:              config.BaseURL,
		httpClient:           httpClient,
		client:               client,
		provisioner:          provisioner,
		metadataGeneration:   metadataGeneration,
		inventoryGeneration:  config.InventoryGeneration,
		runtimeGenerations:   make(map[string]int64),
		runtimeTransitions:   make(map[string]RuntimeTransitionSnapshot),
		cleanupPendings:      make(map[string]struct{}),
		healthStopPendings:   make(map[string]HealthStopSnapshot),
		activeOperations:     make(map[string]*operationContext),
		stateStore:           config.OperationStateStore,
		inventoryStore:       config.InventoryStateStore,
		runtimeStateStore:    config.RuntimeStateStore,
		cleanupStateStore:    config.CleanupStateStore,
		healthStopStateStore: config.HealthStopStateStore,
		runtimeEnvStateStore: config.RuntimeEnvironmentStateStore,
		metadataStateStore:   config.RuntimeMetadataStateStore,
		startupInputStore:    startupInputStore,
		endpointApplier:      config.RuntimeEndpointApplier,
		runtimeHealthStore:   config.RuntimeHealthStateStore,
		metadataPublisher:    config.RuntimeMetadataPublisher,
		runtimeIdentityStore: config.RuntimeIdentityStore,
		sessionTracker:       config.SessionTracker,
		accessController:     config.AccessController,
		settingsStore:        config.ManagerServiceSettings,
		gitSSHKeyType:        gitSSHKeyType,
		autoStops:            make(map[string]*autoStopState),
		healthFailures:       make(map[string]int),
		healthCandidates:     make(map[string]struct{}),
		criticalErrors:       make(chan error, 1),
		capacityCoordinator:  config.CapacityCoordinator,
	}
	for codespaceUUID, generation := range config.InitialRuntimeGenerations {
		if codespaceUUID == "" || generation <= 0 {
			continue
		}
		agent.runtimeGenerations[codespaceUUID] = generation
	}
	for _, transition := range config.InitialRuntimeTransitions {
		if transition.CodespaceUUID == "" || transition.RuntimeGeneration <= 0 {
			continue
		}
		agent.runtimeTransitions[transition.CodespaceUUID] = transition
		if agent.runtimeGenerations[transition.CodespaceUUID] < transition.RuntimeGeneration {
			agent.runtimeGenerations[transition.CodespaceUUID] = transition.RuntimeGeneration
		}
	}
	for _, codespaceUUID := range config.InitialCleanupPendings {
		if codespaceUUID == "" {
			continue
		}
		agent.cleanupPendings[codespaceUUID] = struct{}{}
	}
	for _, pending := range config.InitialHealthStopPendings {
		if pending.CodespaceUUID == "" || pending.ObservedOperationRVersion <= 0 {
			continue
		}
		agent.healthStopPendings[pending.CodespaceUUID] = pending
	}
	for _, snapshot := range config.InitialOperations {
		if snapshot.Payload == nil {
			continue
		}
		codespaceUUID := snapshot.Payload.GetRuntimeUuid()
		operationRVersion := snapshot.Payload.GetOperationRversion()
		if codespaceUUID == "" || operationRVersion <= 0 {
			continue
		}
		agent.activeOperations[codespaceUUID] = &operationContext{
			recoveryBlocked:   snapshot.WorkerStage == OperationWorkerStageRecoveryBlocked,
			operationRVersion: operationRVersion,
			payload:           snapshot.Payload,
			running:           false,
		}
		if snapshot.WorkerStage == OperationWorkerStageRecoveryBlocked {
			log.Printf("operation %s version %d was interrupted with unknown remote effects; awaiting control-plane reconciliation", codespaceUUID, operationRVersion)
		}
	}
	return agent
}

func (a *Agent) managerClient() codespacev1connect.ManagerServiceClient {
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.client
}

func (a *Agent) currentServiceSettings() ManagerServiceSettings {
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.serviceSettings
}

func (a *Agent) saveServiceSettings(settings ManagerServiceSettings) error {
	if a.settingsStore != nil {
		if err := a.settingsStore.SaveManagerServiceSettings(settings); err != nil {
			return fmt.Errorf("save manager service settings: %w", err)
		}
	}
	a.clientMu.Lock()
	if settings.ControlPlaneMaxMessageSize > 0 {
		a.client = controlplane.NewManagerServiceClient(a.httpClient, a.baseURL, a.config.ManagerID, a.config.ManagerSecret, settings.ControlPlaneMaxMessageSize)
	}
	a.serviceSettings = settings
	a.clientMu.Unlock()
	return nil
}

// Run declares the Manager and processes operations until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) (runErr error) {
	if a.startupInputStore == nil {
		return fmt.Errorf("startup input state store is required")
	}
	ctx, cancel := context.WithCancel(ctx)
	shutdownCtx, finishShutdown := context.WithCancel(context.WithoutCancel(ctx))
	if a.config.ExecutionContext != nil {
		stopLost := context.AfterFunc(a.config.ExecutionContext, finishShutdown)
		defer stopLost()
	}
	a.shutdownContext = shutdownCtx
	timeout := a.config.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-shutdownCtx.Done():
			return
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			finishShutdown()
		case <-shutdownCtx.Done():
		}
	}()
	defer func() {
		cancel()
		defer finishShutdown()
		done := make(chan struct{})
		go func() {
			a.operationWorkers.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-shutdownCtx.Done():
			runErr = errors.Join(runErr, fmt.Errorf("wait for operation shutdown: %w", context.DeadlineExceeded))
		}
	}()
	if err := a.runCleanupPendings(ctx); err != nil {
		return runContextError(err)
	}
	if err := a.declareUntilSuccess(ctx, codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_RECOVERING); err != nil {
		return runContextError(err)
	}
	if err := a.runHealthStopPendings(ctx); err != nil {
		return runContextError(err)
	}
	if err := a.reportInventoryUntilSuccess(ctx); err != nil {
		return runContextError(err)
	}
	if err := a.declareUntilSuccess(ctx, codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE); err != nil {
		return runContextError(err)
	}

	inventoryTicker := time.NewTicker(defaultInventoryInterval)
	defer inventoryTicker.Stop()
	pollTicker := time.NewTicker(a.intervalOrDefault(a.config.PollInterval, time.Second))
	defer pollTicker.Stop()
	autoStopTicker := time.NewTicker(a.intervalOrDefault(a.config.PollInterval, time.Second))
	defer autoStopTicker.Stop()
	declareTimer := time.NewTimer(a.currentHeartbeatInterval())
	defer declareTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-a.criticalErrors:
			return err
		case <-declareTimer.C:
			if err := a.declare(ctx, codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE); err != nil {
				if isManagerCriticalError(err) {
					return fmt.Errorf("declare manager: %w", err)
				}
				log.Printf("declare manager: %v", err)
			}
			declareTimer.Reset(a.currentHeartbeatInterval())
		case <-inventoryTicker.C:
			if err := a.reportInventoryOnce(ctx); err != nil {
				if isManagerCriticalError(err) {
					return fmt.Errorf("report instances: %w", err)
				}
				log.Printf("report instances: %v", err)
			}
		case <-pollTicker.C:
			if err := a.pollOnce(ctx); err != nil {
				if isManagerCriticalError(err) {
					return fmt.Errorf("fetch operations: %w", err)
				}
				log.Printf("fetch operations: %v", err)
			}
		case <-autoStopTicker.C:
			if err := a.reconcileAutoStops(ctx); err != nil {
				if isManagerCriticalError(err) {
					return fmt.Errorf("auto stop: %w", err)
				}
				log.Printf("auto stop: %v", err)
			}
		}
	}
}

func (a *Agent) reportInventoryUntilSuccess(ctx context.Context) error {
	interval := a.intervalOrDefault(a.config.DeclareInterval, 5*time.Second)
	for {
		if err := a.reportInventoryOnce(ctx); err != nil {
			if isManagerCriticalError(err) {
				return fmt.Errorf("report instances: %w", err)
			}
			log.Printf("report instances: %v", err)
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		return nil
	}
}

func (a *Agent) declareUntilSuccess(ctx context.Context, state codespacev1.ManagerRuntimeState) error {
	interval := a.intervalOrDefault(a.config.DeclareInterval, 5*time.Second)
	for {
		if err := a.declare(ctx, state); err != nil {
			if isManagerCriticalError(err) {
				return fmt.Errorf("declare %s: %w", strings.ToLower(state.String()), err)
			}
			log.Printf("declare %s: %v", strings.ToLower(state.String()), err)
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		return nil
	}
}

func (a *Agent) currentHeartbeatInterval() time.Duration {
	settings := a.currentServiceSettings()
	if settings.HeartbeatInterval > 0 {
		return settings.HeartbeatInterval
	}
	return a.intervalOrDefault(a.config.DeclareInterval, 5*time.Second)
}

func runContextError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (a *Agent) declare(ctx context.Context, state codespacev1.ManagerRuntimeState) error {
	request := connect.NewRequest(&codespacev1.DeclareManagerRequest{
		ProtocolVersion:                    controlplane.ProtocolVersion,
		GatewayUrl:                         a.config.GatewayURL,
		GatewaySshAddr:                     a.config.GatewaySSHAddr,
		Environments:                       a.config.Environments,
		Version:                            a.config.Version,
		ManagerRuntimeState:                state,
		GatewaySshHostKeyAlgorithm:         a.config.GatewaySSHHostKeyAlgo,
		GatewaySshHostKeyFingerprintSha256: a.config.GatewaySSHHostKeySHA256,
		GatewaySshHostKeyUpdatedUnix:       a.config.GatewaySSHHostKeyUnix,
	})
	response, err := a.managerClient().DeclareManager(ctx, request)
	if err != nil {
		return fmt.Errorf("declare rpc: %w", err)
	}
	settings, err := validateDeclareResponse(response.Msg)
	if err != nil {
		return err
	}
	if err := a.saveServiceSettings(settings); err != nil {
		return err
	}
	return nil
}

func (a *Agent) pollOnce(ctx context.Context) error {
	requestStarted := time.Now()
	requestOperationVersions := a.currentOperationVersions()
	instances, listErr := a.provisioner.ListInstances(ctx)
	capacity := a.reserveFetchCapacity(instances, listErr)
	started := fetchCapacity{}
	defer func() { a.releaseFetchReservation(capacity, started) }()
	capacity = a.applyStartupAdmission(ctx, capacity)
	request := connect.NewRequest(&codespacev1.FetchOperationsRequest{
		ProtocolVersion:          controlplane.ProtocolVersion,
		StartupCapacityAvailable: capacity.startup,
		AcceptedOperationTypes:   capacity.acceptedOperationTypes(),
		ObservedOperations:       a.observedOperations(),
		CleanupCapacityAvailable: capacity.cleanup,
		AcceptedCreateTags:       capacity.acceptedCreateTags,
	})
	response, err := a.managerClient().FetchOperations(ctx, request)
	if err != nil {
		return fmt.Errorf("fetch operations rpc: %w", err)
	}
	for _, operation := range response.Msg.GetOperations() {
		if operation == nil {
			continue
		}
		ok, err := a.validateOperationResponseVersion("fetch operation", operation.GetRuntimeUuid(), requestOperationVersions, operation.GetOperationRversion())
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		duration := operationLeaseDurationFromRequestStart(requestStarted, operation)
		if err := a.startOperation(ctx, operation, duration); err != nil {
			return fmt.Errorf("start operation %s version %d: %w", operation.GetRuntimeUuid(), operation.GetOperationRversion(), err)
		}
		switch operation.GetCommand().(type) {
		case *codespacev1.OperationPayload_Create, *codespacev1.OperationPayload_Resume:
			started.startup++
		case *codespacev1.OperationPayload_Stop, *codespacev1.OperationPayload_Delete,
			*codespacev1.OperationPayload_AbortCreate, *codespacev1.OperationPayload_AbortResume:
			started.cleanup++
		}
	}
	for _, lease := range response.Msg.GetRenewedLeases() {
		if lease == nil {
			continue
		}
		ok, err := a.validateOperationResponseVersion("fetch renewed lease", lease.GetRuntimeUuid(), requestOperationVersions, lease.GetOperationRversion())
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		duration := leaseDurationFromRequestStart(requestStarted, lease.GetLeaseValidForMilliseconds())
		if err := a.resumeRenewedOperation(ctx, lease, duration); err != nil {
			return fmt.Errorf("resume renewed operation %s version %d: %w", lease.GetRuntimeUuid(), lease.GetOperationRversion(), err)
		}
	}
	return nil
}

func (a *Agent) applyStartupAdmission(ctx context.Context, capacity fetchCapacity) fetchCapacity {
	if capacity.startup <= 0 {
		return capacity
	}
	checker, ok := a.provisioner.(provisioner.StartupAdmissionChecker)
	if !ok {
		return capacity
	}
	admission, err := checker.CheckStartupAdmission(ctx)
	if err != nil {
		log.Printf("check startup admission: %v", err)
		capacity.acceptedCreateTags = nil
		capacity.acceptResume = true
		return capacity
	}
	capacity.acceptedCreateTags = append([]string(nil), admission.CreateTags...)
	capacity.acceptResume = admission.ResumeAvailable
	return capacity
}

type fetchCapacity struct {
	startup            int32
	cleanup            int32
	acceptedCreateTags []string
	acceptResume       bool
}

func (c fetchCapacity) acceptedOperationTypes() []codespacev1.AcceptedOperationType {
	if c.startup <= 0 {
		return nil
	}
	types := make([]codespacev1.AcceptedOperationType, 0, 2)
	if len(c.acceptedCreateTags) > 0 {
		types = append(types, codespacev1.AcceptedOperationType_ACCEPTED_OPERATION_TYPE_CREATE)
	}
	if c.acceptResume {
		types = append(types, codespacev1.AcceptedOperationType_ACCEPTED_OPERATION_TYPE_RESUME)
	}
	return types
}

func (a *Agent) fetchCapacity(instances []*provisioner.Instance, listErr error) fetchCapacity {
	if listErr != nil {
		return fetchCapacity{}
	}
	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	return a.fetchCapacityLocked(instances)
}

func (a *Agent) reserveFetchCapacity(instances []*provisioner.Instance, listErr error) fetchCapacity {
	if listErr != nil {
		return fetchCapacity{}
	}
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	capacity := a.fetchCapacityLocked(instances)
	if a.capacityCoordinator != nil {
		snapshot := a.operationCapacitySnapshotLocked()
		observation := CapacityObservation{
			RuntimeOccupied: int32(len(runtimeCapacityOccupants(instances, snapshot.startup))),
			StartupActive:   int32(len(snapshot.startup)),
			CleanupActive:   int32(len(snapshot.cleanup) + len(snapshot.cleanupPendings)),
		}
		reserved := a.capacityCoordinator.Reserve(a.config.CapacitySiteID, observation, CapacityReservation{Startup: capacity.startup, Cleanup: capacity.cleanup})
		capacity.startup = reserved.Startup
		capacity.cleanup = reserved.Cleanup
	}
	a.fetchReservedStartup += capacity.startup
	a.fetchReservedCleanup += capacity.cleanup
	return capacity
}

func (a *Agent) releaseFetchReservation(capacity, started fetchCapacity) {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	a.fetchReservedStartup = max(0, a.fetchReservedStartup-capacity.startup)
	a.fetchReservedCleanup = max(0, a.fetchReservedCleanup-capacity.cleanup)
	if a.capacityCoordinator != nil {
		a.capacityCoordinator.Release(a.config.CapacitySiteID,
			CapacityReservation{Startup: capacity.startup, Cleanup: capacity.cleanup},
			CapacityReservation{Startup: started.startup, Cleanup: started.cleanup})
	}
}

func (a *Agent) fetchCapacityLocked(instances []*provisioner.Instance) fetchCapacity {
	snapshot := a.operationCapacitySnapshotLocked()
	runtimeSlots := max(0, a.runtimeSlotsAvailable(instances, snapshot.startup)-a.fetchReservedStartup)
	startupSlots := max(0, a.startupWorkers()-int32(len(snapshot.startup))-a.fetchReservedStartup)
	cleanupSlots := max(0, a.cleanupWorkers()-int32(len(snapshot.cleanup))-int32(len(snapshot.cleanupPendings))-a.fetchReservedCleanup)
	acceptedCreateTags := make([]string, 0, len(a.config.Environments))
	for _, environment := range a.config.Environments {
		acceptedCreateTags = append(acceptedCreateTags, environment.GetTag())
	}
	return fetchCapacity{
		startup:            min(runtimeSlots, startupSlots),
		cleanup:            cleanupSlots,
		acceptedCreateTags: acceptedCreateTags,
		acceptResume:       true,
	}
}

type operationCapacitySnapshot struct {
	startup         map[string]struct{}
	cleanup         map[string]struct{}
	cleanupPendings map[string]struct{}
}

func (a *Agent) operationCapacitySnapshotLocked() operationCapacitySnapshot {
	snapshot := operationCapacitySnapshot{
		startup:         make(map[string]struct{}),
		cleanup:         make(map[string]struct{}),
		cleanupPendings: make(map[string]struct{}, len(a.cleanupPendings)),
	}
	for codespaceUUID, operation := range a.activeOperations {
		if operation == nil || !operation.running || operation.payload == nil {
			continue
		}
		switch operation.payload.GetCommand().(type) {
		case *codespacev1.OperationPayload_Create, *codespacev1.OperationPayload_Resume:
			snapshot.startup[codespaceUUID] = struct{}{}
		case *codespacev1.OperationPayload_Stop,
			*codespacev1.OperationPayload_Delete,
			*codespacev1.OperationPayload_AbortCreate,
			*codespacev1.OperationPayload_AbortResume:
			snapshot.cleanup[codespaceUUID] = struct{}{}
		}
	}
	for codespaceUUID := range a.cleanupPendings {
		if _, active := snapshot.cleanup[codespaceUUID]; active {
			continue
		}
		snapshot.cleanupPendings[codespaceUUID] = struct{}{}
	}
	return snapshot
}

func (a *Agent) runtimeSlotsAvailable(instances []*provisioner.Instance, activeStartup map[string]struct{}) int32 {
	return max(0, a.config.CapacityTotal-int32(len(runtimeCapacityOccupants(instances, activeStartup))))
}

func runtimeCapacityOccupants(instances []*provisioner.Instance, activeStartup map[string]struct{}) map[string]struct{} {
	occupied := make(map[string]struct{}, len(instances)+len(activeStartup))
	for _, instance := range instances {
		if instance == nil || instance.CodespaceUUID == "" {
			continue
		}
		switch instance.RuntimeState {
		case provisioner.RuntimeStateCreating, provisioner.RuntimeStateRunning:
			occupied[instance.CodespaceUUID] = struct{}{}
		}
	}
	for codespaceUUID := range activeStartup {
		occupied[codespaceUUID] = struct{}{}
	}
	return occupied
}

func (a *Agent) startupWorkers() int32 {
	if a.config.StartupWorkers > 0 {
		return a.config.StartupWorkers
	}
	if a.config.CapacityTotal > 0 && a.config.CapacityTotal < 4 {
		return a.config.CapacityTotal
	}
	return 4
}

func (a *Agent) cleanupWorkers() int32 {
	if a.config.CleanupWorkers > 0 {
		return a.config.CleanupWorkers
	}
	return 4
}

func (a *Agent) observedOperations() []*codespacev1.ObservedOperation {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	observed := make([]*codespacev1.ObservedOperation, 0, len(a.activeOperations))
	for codespaceUUID, operation := range a.activeOperations {
		if operation.payload == nil || operation.operationRVersion <= 0 || operation.recoveryBlocked {
			continue
		}
		observed = append(observed, &codespacev1.ObservedOperation{
			RuntimeUuid:       codespaceUUID,
			OperationRversion: operation.operationRVersion,
		})
	}
	return observed
}

func (a *Agent) startOperation(ctx context.Context, operation *codespacev1.OperationPayload, leaseDuration time.Duration) error {
	if operation == nil {
		return nil
	}
	if operation.GetRuntimeUuid() == "" && operation.GetCreate() != nil {
		runtimeUUID, err := a.bindCreateRuntimeIdentity(ctx, operation)
		if err != nil {
			return err
		}
		operation.RuntimeUuid = runtimeUUID
	}
	codespaceUUID := operation.GetRuntimeUuid()
	operationRVersion := operation.GetOperationRversion()
	if codespaceUUID == "" || operationRVersion <= 0 {
		log.Printf("skip invalid operation %q version %d", codespaceUUID, operationRVersion)
		return nil
	}

	a.activeMu.Lock()
	current, ok := a.activeOperations[codespaceUUID]
	if ok {
		if current.recoveryBlocked {
			a.activeMu.Unlock()
			return nil
		}
		if current.operationRVersion > operationRVersion {
			a.activeMu.Unlock()
			log.Printf("skip operation %s version %d while version %d is active", codespaceUUID, operationRVersion, current.operationRVersion)
			return nil
		}
		if current.operationRVersion < operationRVersion {
			if !isDeleteOperation(operation) {
				a.activeMu.Unlock()
				return &categorizedError{
					category: failureProtocolMismatch,
					message:  fmt.Sprintf("operation %s version %d cannot replace active version %d without delete", codespaceUUID, operationRVersion, current.operationRVersion),
				}
			}
			if err := a.saveOperationState(operation, OperationWorkerStageActive); err != nil {
				a.activeMu.Unlock()
				return err
			}
			a.stopLeaseLocked(current)
			operationContext := &operationContext{
				operationRVersion: operationRVersion,
				payload:           operation,
				running:           true,
			}
			operationCtx := a.startLeaseLocked(ctx, codespaceUUID, operationContext, leaseDuration)
			a.activeOperations[codespaceUUID] = operationContext
			a.activeMu.Unlock()

			if err := a.deactivateRuntimeMetadata(ctx, codespaceUUID); err != nil {
				return err
			}
			a.runOperation(operationCtx, operation)
			return nil
		}
		if current.running {
			if canAbortRunningOperation(current.payload, operation) {
				current.payload = operation
				a.stopLeaseLocked(current)
				current.running = false
			} else {
				a.activeMu.Unlock()
				return nil
			}
		}
	}
	a.activeMu.Unlock()

	if err := a.saveOperationState(operation, OperationWorkerStageActive); err != nil {
		return err
	}

	a.activeMu.Lock()
	if current, ok := a.activeOperations[codespaceUUID]; ok {
		if current.operationRVersion != operationRVersion {
			a.activeMu.Unlock()
			log.Printf("skip operation %s version %d while version %d is active", codespaceUUID, operationRVersion, current.operationRVersion)
			return nil
		}
		if current.running {
			a.activeMu.Unlock()
			return nil
		}
		current.payload = operation
		current.running = true
		operationCtx := a.startLeaseLocked(ctx, codespaceUUID, current, leaseDuration)
		a.activeMu.Unlock()
		a.runOperation(operationCtx, operation)
		return nil
	}
	operationContext := &operationContext{
		operationRVersion: operationRVersion,
		payload:           operation,
		running:           true,
	}
	operationCtx := a.startLeaseLocked(ctx, codespaceUUID, operationContext, leaseDuration)
	a.activeOperations[codespaceUUID] = operationContext
	a.activeMu.Unlock()

	a.runOperation(operationCtx, operation)
	return nil
}

func (a *Agent) bindCreateRuntimeIdentity(ctx context.Context, operation *codespacev1.OperationPayload) (string, error) {
	if operation.GetCodespaceId() <= 0 || operation.GetOperationRversion() <= 0 {
		return "", fmt.Errorf("create operation is missing codespace identity")
	}
	runtimeUUID := uuid.NewString()
	request := connect.NewRequest(&codespacev1.BindRuntimeIdentityRequest{
		ProtocolVersion:   controlplane.ProtocolVersion,
		CodespaceId:       operation.GetCodespaceId(),
		OperationRversion: operation.GetOperationRversion(),
		RuntimeUuid:       runtimeUUID,
	})
	response, err := a.managerClient().BindRuntimeIdentity(ctx, request)
	if err != nil {
		return "", fmt.Errorf("bind runtime identity rpc: %w", err)
	}
	if response.Msg.GetRuntimeUuid() != runtimeUUID {
		return "", fmt.Errorf("bind runtime identity returned unexpected uuid")
	}
	if a.runtimeIdentityStore != nil {
		if err := a.runtimeIdentityStore.SaveRuntimeIdentity(
			ctx, runtimeUUID, operation.GetCodespaceId(), operation.GetOperationRversion(), operation.GetCreate().GetEnvironmentTag(),
		); err != nil {
			return "", &categorizedError{category: failureLocalStateCommit, message: fmt.Sprintf("save runtime identity: %v", err)}
		}
	}
	return runtimeUUID, nil
}

func (a *Agent) saveOperationState(operation *codespacev1.OperationPayload, stage OperationWorkerStage) error {
	if a.stateStore == nil {
		return nil
	}
	return a.stateStore.SaveActiveOperation(OperationSnapshot{Payload: operation, WorkerStage: stage})
}

func (a *Agent) resumeRenewedOperation(ctx context.Context, lease *codespacev1.RenewedOperationLease, leaseDuration time.Duration) error {
	if lease == nil || lease.GetRuntimeUuid() == "" || lease.GetOperationRversion() <= 0 {
		return nil
	}
	a.activeMu.Lock()
	current, ok := a.activeOperations[lease.GetRuntimeUuid()]
	if !ok || current.operationRVersion != lease.GetOperationRversion() || current.payload == nil {
		a.activeMu.Unlock()
		return nil
	}
	if current.running {
		a.resetLeaseTimerLocked(lease.GetRuntimeUuid(), current, leaseDuration)
		a.activeMu.Unlock()
		return nil
	}
	current.running = true
	payload := current.payload
	operationCtx := a.startLeaseLocked(ctx, lease.GetRuntimeUuid(), current, leaseDuration)
	a.activeMu.Unlock()

	a.runOperation(operationCtx, payload)
	return nil
}

func (a *Agent) runOperation(ctx context.Context, operation *codespacev1.OperationPayload) {
	codespaceUUID := operation.GetRuntimeUuid()
	operationRVersion := operation.GetOperationRversion()
	a.operationWorkers.Add(1)
	go func() {
		defer a.operationWorkers.Done()
		if err := a.handleOperation(ctx, operation); err != nil {
			critical := isManagerCriticalError(err)
			if ctx.Err() != nil && isStartupOperation(operation) {
				cleanupCtx, cancel := a.newCleanupContext()
				defer cancel()
				if deactivateErr := a.deactivateRuntimeMetadata(cleanupCtx, codespaceUUID); deactivateErr != nil {
					log.Printf("deactivate paused operation %s version %d: %v", codespaceUUID, operationRVersion, deactivateErr)
				}
				if stopErr := a.provisioner.Stop(cleanupCtx, runtimeInstanceName(codespaceUUID)); stopErr != nil {
					log.Printf("stop paused operation %s version %d: %v", codespaceUUID, operationRVersion, stopErr)
				}
			}
			a.pauseOperation(codespaceUUID, operationRVersion, operation)
			log.Printf("handle operation %s version %d: %v", codespaceUUID, operationRVersion, err)
			if critical {
				a.reportCriticalError(fmt.Errorf("operation %s version %d: %w", codespaceUUID, operationRVersion, err))
			}
			return
		}
		a.finishOperation(codespaceUUID, operationRVersion, operation)
	}()
}

func (a *Agent) reportCriticalError(err error) {
	select {
	case a.criticalErrors <- err:
	default:
	}
}

func (a *Agent) finishOperation(codespaceUUID string, operationRVersion int64, operation *codespacev1.OperationPayload) {
	a.activeMu.Lock()
	matched := false
	if current, ok := a.activeOperations[codespaceUUID]; ok && current.operationRVersion == operationRVersion && current.payload == operation {
		a.stopLeaseLocked(current)
		delete(a.activeOperations, codespaceUUID)
		matched = true
	}
	a.activeMu.Unlock()

	if matched && a.stateStore != nil {
		if err := a.stateStore.DeleteActiveOperation(codespaceUUID, operationRVersion); err != nil {
			log.Printf("delete operation state %s version %d: %v", codespaceUUID, operationRVersion, err)
		}
	}
}

func (a *Agent) pauseOperation(codespaceUUID string, operationRVersion int64, operation *codespacev1.OperationPayload) {
	a.activeMu.Lock()
	current, ok := a.activeOperations[codespaceUUID]
	if ok && current.operationRVersion == operationRVersion && current.payload == operation {
		a.stopLeaseLocked(current)
		current.running = false
	}
	var payload *codespacev1.OperationPayload
	if ok && current.operationRVersion == operationRVersion && current.payload == operation {
		payload = current.payload
	}
	a.activeMu.Unlock()

	if payload != nil {
		if err := a.saveOperationState(payload, OperationWorkerStageLeasePaused); err != nil {
			log.Printf("pause operation state %s version %d: %v", codespaceUUID, operationRVersion, err)
		}
	}
}

func (a *Agent) startLeaseLocked(ctx context.Context, codespaceUUID string, operation *operationContext, leaseDuration time.Duration) context.Context {
	a.stopLeaseLocked(operation)
	operationCtx, cancel := context.WithCancel(ctx)
	operation.cancel = cancel
	if leaseDuration > 0 {
		operation.leaseTimer = time.AfterFunc(leaseDuration, func() {
			log.Printf("operation %s version %d local lease expired", codespaceUUID, operation.operationRVersion)
			cancel()
		})
	}
	return operationCtx
}

func (a *Agent) resetLeaseTimerLocked(codespaceUUID string, operation *operationContext, leaseDuration time.Duration) {
	if operation.leaseTimer == nil {
		return
	}
	operation.leaseTimer.Stop()
	operation.leaseTimer.Reset(leaseDuration)
}

func (a *Agent) stopLeaseLocked(operation *operationContext) {
	if operation.leaseTimer != nil {
		operation.leaseTimer.Stop()
		operation.leaseTimer = nil
	}
	if operation.cancel != nil {
		operation.cancel()
		operation.cancel = nil
	}
}

func leaseDurationFromRequestStart(requestStarted time.Time, leaseMillis int64) time.Duration {
	if leaseMillis <= 0 {
		return time.Nanosecond
	}
	deadline := requestStarted.Add(time.Duration(leaseMillis) * time.Millisecond)
	duration := time.Until(deadline)
	if duration <= 0 {
		return time.Nanosecond
	}
	return duration
}

func operationLeaseDurationFromRequestStart(requestStarted time.Time, operation *codespacev1.OperationPayload) time.Duration {
	if isAbortOperation(operation) {
		return 0
	}
	return leaseDurationFromRequestStart(requestStarted, operation.GetLeaseValidForMilliseconds())
}

func (a *Agent) handleOperation(ctx context.Context, operation *codespacev1.OperationPayload) error {
	if err := a.updateLog(ctx, operation, logGroupStartPrefix+operationLogGroupName(operation)); err != nil {
		return err
	}
	if isAbortOperation(operation) {
		if err := a.updateLog(ctx, operation, "##[warning]Gitea requested cancellation of this startup operation."); err != nil {
			return err
		}
	}

	var err error
	switch command := operation.GetCommand().(type) {
	case *codespacev1.OperationPayload_Create:
		err = a.handleCreate(ctx, operation, command.Create)
	case *codespacev1.OperationPayload_Resume:
		err = a.handleResume(ctx, operation, command.Resume)
	case *codespacev1.OperationPayload_Stop:
		err = a.handleStop(ctx, operation)
	case *codespacev1.OperationPayload_Delete:
		err = a.handleDelete(ctx, operation)
	case *codespacev1.OperationPayload_AbortCreate:
		err = a.handleDelete(ctx, operation)
	case *codespacev1.OperationPayload_AbortResume:
		err = a.handleStop(ctx, operation)
	default:
		err = fmt.Errorf("operation command is missing")
	}

	finalStatus := codespacev1.FinalStatus_FINAL_STATUS_DONE
	if isAbortOperation(operation) {
		finalStatus = codespacev1.FinalStatus_FINAL_STATUS_FAILED
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isManagerCriticalError(err) {
			return err
		}
		if isStartupOperation(operation) && isRuntimeMetadataHardFailure(err) {
			return a.handleRuntimeMetadataHardFailure(ctx, operation, err)
		}
		if logErr := a.updateLog(ctx, operation, logErrorPrefix+err.Error()); isManagerCriticalError(logErr) {
			return logErr
		}
		if logErr := a.updateLog(ctx, operation, logGroupEnd); logErr != nil {
			return logErr
		}
		a.closeCodespaceAccess(operation.GetRuntimeUuid())
		if provisioner.IsRecoverableRuntimeFailure(err) {
			return err
		}
		finalStatus = codespacev1.FinalStatus_FINAL_STATUS_FAILED
	} else if logErr := a.updateLog(ctx, operation, logGroupEnd); logErr != nil {
		return logErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	outcome, finalizeErr := a.finalize(ctx, operation, finalStatus, operationType(operation))
	if err != nil {
		if finalizeErr != nil {
			if isManagerCriticalError(finalizeErr) {
				return finalizeErr
			}
			return fmt.Errorf("%w; finalize failed: %v", err, finalizeErr)
		}
		if outcome == finalizeOutcomeResourceAbsent {
			a.handleResourceAbsentFinal(ctx, operation)
		}
		return nil
	}
	if finalizeErr != nil {
		return finalizeErr
	}
	if outcome == finalizeOutcomeResourceAbsent {
		a.handleResourceAbsentFinal(ctx, operation)
	}
	if isDeleteOperation(operation) {
		return a.clearDeleteCleanupState(ctx, operation.GetRuntimeUuid())
	}
	return nil
}

func (a *Agent) handleRuntimeMetadataHardFailure(ctx context.Context, operation *codespacev1.OperationPayload, err error) error {
	codespaceUUID := operation.GetRuntimeUuid()
	if logErr := a.updateLog(ctx, operation, logErrorPrefix+err.Error()); isManagerCriticalError(logErr) {
		return logErr
	}
	if logErr := a.updateLog(ctx, operation, logGroupEnd); logErr != nil {
		return logErr
	}
	a.closeCodespaceAccess(codespaceUUID)
	outcome, finalizeErr := a.finalize(ctx, operation, codespacev1.FinalStatus_FINAL_STATUS_FAILED, operationType(operation))
	if finalizeErr != nil {
		return finalizeErr
	}
	if outcome == finalizeOutcomeResourceAbsent {
		a.handleResourceAbsentFinal(ctx, operation)
	}
	if err := a.saveCleanupPending(codespaceUUID); err != nil {
		return err
	}
	if err := a.cleanupLocalRuntime(ctx, codespaceUUID); err != nil {
		return err
	}
	return nil
}

func operationLogGroupName(operation *codespacev1.OperationPayload) string {
	name := strings.ToLower(operationType(operation).String())
	name = strings.TrimPrefix(name, "operation_type_")
	if name != "" {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return fmt.Sprintf("%s #%d", name, operation.GetOperationRversion())
}

func isRuntimeMetadataHardFailure(err error) bool {
	switch failureCategory(err) {
	case failureGenerationConflict, failureVersionExhausted:
		return true
	default:
		return false
	}
}

func (a *Agent) handleResourceAbsentFinal(ctx context.Context, operation *codespacev1.OperationPayload) {
	if ctx.Err() != nil {
		return
	}
	a.finishOperation(operation.GetRuntimeUuid(), operation.GetOperationRversion(), operation)
	cleanupCtx, cancel := a.newCleanupContext()
	defer cancel()
	a.triggerResourceAbsentInventory(cleanupCtx, operation)
}

func (a *Agent) syncRuntimeEndpointManifest(ctx context.Context, codespaceUUID string, instance *provisioner.Instance) error {
	if a.endpointApplier == nil {
		return nil
	}
	if instance == nil {
		return fmt.Errorf("runtime instance is required")
	}
	declarations, err := a.provisioner.ReadEndpointManifest(ctx, instance.Name)
	if err != nil {
		return fmt.Errorf("read runtime endpoint manifest: %w", err)
	}
	if len(declarations) > runtimeendpoint.MaxDeclaredEndpointCount {
		return fmt.Errorf("runtime endpoint manifest exceeds limit %d", runtimeendpoint.MaxDeclaredEndpointCount)
	}
	routes := make([]RuntimeEndpointRoute, 0, len(declarations)+1)
	routes = append(routes, RuntimeEndpointRoute{
		CodespaceUUID: codespaceUUID,
		EndpointID:    runtimeendpoint.WorkspaceEndpointID,
		Label:         runtimeendpoint.WorkspaceEndpointLabel,
		InstanceName:  instance.Name,
		UpstreamPort:  runtimeendpoint.WorkspaceEndpointPort,
	})
	for _, declaration := range declarations {
		if declaration.UpstreamPort < 1 || declaration.UpstreamPort > 65535 {
			return fmt.Errorf("endpoint %s upstream_port is invalid", declaration.EndpointID)
		}
		expectedID := fmt.Sprintf("port-%d", declaration.UpstreamPort)
		if declaration.EndpointID != expectedID {
			return fmt.Errorf("endpoint %s must use id %s", declaration.EndpointID, expectedID)
		}
		routes = append(routes, RuntimeEndpointRoute{
			CodespaceUUID: codespaceUUID,
			EndpointID:    declaration.EndpointID,
			Label:         declaration.Label,
			InstanceName:  instance.Name,
			UpstreamPort:  uint32(declaration.UpstreamPort),
			Public:        declaration.Public,
		})
	}
	if err := a.endpointApplier.ApplyRuntimeEndpointRoutes(ctx, codespaceUUID, routes); err != nil {
		return fmt.Errorf("apply runtime endpoint routes: %w", err)
	}
	return nil
}

func (a *Agent) saveRuntimeEnvironment(codespaceUUID string, environment provisioner.RuntimeEnvironment) error {
	if a.runtimeEnvStateStore == nil {
		return nil
	}
	if err := a.runtimeEnvStateStore.SaveRuntimeEnvironment(codespaceUUID, environment); err != nil {
		return &categorizedError{
			category: failureLocalStateCommit,
			message:  fmt.Sprintf("save runtime environment %s: %v", codespaceUUID, err),
		}
	}
	return nil
}

func (a *Agent) validateRuntimeWorkspaceAccess(ctx context.Context, instance *provisioner.Instance) error {
	if instance == nil {
		return fmt.Errorf("runtime instance is nil")
	}
	checker, ok := a.provisioner.(workspaceAccessChecker)
	if !ok {
		return nil
	}
	return a.checkRuntimeWorkspaceAccess(ctx, checker, RuntimeMetadataSnapshot{
		InstanceName: instance.Name,
		Workdir:      instance.Workdir,
	})
}

func (a *Agent) validateRuntimeReady(ctx context.Context, codespaceUUID string, instance *provisioner.Instance) error {
	if instance == nil {
		return fmt.Errorf("runtime instance is nil")
	}
	if !filepath.IsAbs(strings.TrimSpace(instance.Workdir)) {
		return fmt.Errorf("runtime workspace path must be absolute")
	}
	if err := a.validateRuntimeWorkspaceAccess(ctx, instance); err != nil {
		return err
	}
	checker, ok := a.provisioner.(workspaceGitChecker)
	if ok {
		status, err := checker.CheckWorkspaceGit(ctx, instance.Name, instance.Workdir)
		if err != nil {
			return fmt.Errorf("check workspace git %s: %w", codespaceUUID, err)
		}
		if !status.CredentialConfigured {
			return fmt.Errorf("workspace git credentials are not configured for origin %q", status.OriginURL)
		}
	}
	return a.checkRuntimeDevelopmentEnvironment(ctx, codespaceUUID, instance.Name)
}

func (a *Agent) checkRuntimeDevelopmentEnvironment(ctx context.Context, codespaceUUID, instanceName string) error {
	checker, ok := a.provisioner.(runtimeDevelopmentEnvironmentChecker)
	if !ok || a.runtimeEnvStateStore == nil {
		return nil
	}
	_, ok, err := a.runtimeEnvStateStore.LoadRuntimeEnvironment(codespaceUUID)
	if err != nil {
		return fmt.Errorf("load runtime environment %s: %w", codespaceUUID, err)
	}
	if !ok {
		return fmt.Errorf("runtime environment is missing")
	}
	if err := checker.CheckDevContainer(ctx, instanceName); err != nil {
		return fmt.Errorf("check Dev Container %s: %w", codespaceUUID, err)
	}
	port := uint16(runtimeendpoint.WorkspaceEndpointPort)
	if err := checker.CheckWorkspaceIDE(ctx, instanceName, uint32(port)); err != nil {
		return fmt.Errorf("check Web IDE %s: %w", codespaceUUID, err)
	}
	return nil
}

func (a *Agent) checkRuntimeWorkspaceAccess(ctx context.Context, checker workspaceAccessChecker, snapshot RuntimeMetadataSnapshot) error {
	instanceName := strings.TrimSpace(snapshot.InstanceName)
	if instanceName == "" {
		return fmt.Errorf("runtime instance name is missing")
	}
	workdir := strings.TrimSpace(snapshot.Workdir)
	if !filepath.IsAbs(workdir) {
		return fmt.Errorf("runtime workspace path must be absolute")
	}
	if err := checker.CheckWorkspaceAccess(ctx, instanceName, workdir); err != nil {
		return fmt.Errorf("check workspace access %s: %w", snapshot.CodespaceUUID, err)
	}
	return nil
}

func (a *Agent) handleStop(ctx context.Context, operation *codespacev1.OperationPayload) error {
	if err := a.deactivateRuntimeMetadata(ctx, operation.GetRuntimeUuid()); err != nil {
		return err
	}
	logSink := newOperationLogSink(a, operation)
	defer func() {
		flushCtx := context.WithoutCancel(ctx)
		logSink.closeGroups(flushCtx)
		_ = logSink.FlushLifecycleLog(flushCtx)
	}()
	stopRequest := provisioner.LifecycleRequest{
		CodespaceUUID: operation.GetRuntimeUuid(),
		CodespaceName: runtimeInstanceName(operation.GetRuntimeUuid()),
		Operation:     provisioner.LifecycleOperationStop,
		LogSink:       logSink,
	}
	if environment, err := a.loadRuntimeEnvironment(operation.GetRuntimeUuid()); err == nil {
		stopRequest.Workdir = environment.Environment.Workspace
		stopRequest.Environment = &environment.Environment
	} else {
		log.Printf("skip runtime stop context for %s: %v", operation.GetRuntimeUuid(), err)
	}
	if result, err := a.provisioner.StopEnvironment(ctx, runtimeInstanceName(operation.GetRuntimeUuid()), stopRequest); err != nil {
		log.Printf("codespace runtime stop failed for %s: %v", operation.GetRuntimeUuid(), err)
	} else if current, loadErr := a.loadRuntimeEnvironment(operation.GetRuntimeUuid()); loadErr == nil {
		current.Environment = result.Environment
		if err := a.saveRuntimeEnvironment(operation.GetRuntimeUuid(), current); err != nil {
			return err
		}
	} else {
		return loadErr
	}
	if err := a.provisioner.ClearRuntimeSecrets(ctx, runtimeInstanceName(operation.GetRuntimeUuid())); err != nil {
		return err
	}
	if err := a.provisioner.Stop(ctx, runtimeInstanceName(operation.GetRuntimeUuid())); err != nil {
		return err
	}
	a.markRuntimeStopped(operation.GetRuntimeUuid())
	return nil
}

func (a *Agent) handleDelete(ctx context.Context, operation *codespacev1.OperationPayload) error {
	if err := a.deactivateRuntimeMetadata(ctx, operation.GetRuntimeUuid()); err != nil {
		return err
	}
	if err := a.saveCleanupPending(operation.GetRuntimeUuid()); err != nil {
		return err
	}
	if err := a.provisioner.Delete(ctx, runtimeInstanceName(operation.GetRuntimeUuid())); err != nil {
		return err
	}
	a.markRuntimeRemoved(operation.GetRuntimeUuid())
	return nil
}

func (a *Agent) closeCodespaceAccess(codespaceUUID string) {
	if a.accessController == nil || codespaceUUID == "" {
		return
	}
	a.accessController.CloseCodespaceAccess(codespaceUUID)
}

func (a *Agent) deactivateRuntimeMetadata(ctx context.Context, codespaceUUID string) error {
	if codespaceUUID == "" {
		return nil
	}
	a.closeCodespaceAccess(codespaceUUID)
	var cleanupErrors []error
	if a.endpointApplier != nil {
		if err := a.endpointApplier.ApplyRuntimeEndpointRoutes(ctx, codespaceUUID, nil); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("clear runtime endpoint routes %s: %w", codespaceUUID, err))
		}
	}
	if a.metadataStateStore != nil {
		if err := a.metadataStateStore.ClearRuntimeMetadata(codespaceUUID); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("clear runtime metadata %s: %w", codespaceUUID, err))
		}
	}
	if a.metadataPublisher != nil {
		a.metadataPublisher.DeactivateRuntimeMetadata(ctx, codespaceUUID)
	}
	return errors.Join(cleanupErrors...)
}

func (a *Agent) requestRuntimeAccess(ctx context.Context, codespaceUUID string, operationRVersion int64, gitSSHPublicKey []byte) (*codespacev1.RuntimeAccessBundle, error) {
	request := connect.NewRequest(&codespacev1.RequestRuntimeAccessRequest{
		ProtocolVersion:   controlplane.ProtocolVersion,
		RuntimeUuid:       codespaceUUID,
		OperationRversion: operationRVersion,
		GitSshKey:         &codespacev1.RuntimeGitSSHKey{PublicKey: gitSSHPublicKey},
	})
	response, err := a.managerClient().RequestRuntimeAccess(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("request runtime access rpc: %w", err)
	}
	if response.Msg.GetAccess() == nil {
		return nil, fmt.Errorf("request runtime access rpc: response access bundle is missing")
	}
	return response.Msg.GetAccess(), nil
}

func (a *Agent) requestIdleStop(
	ctx context.Context,
	codespaceUUID string,
	runtimeSettings *codespacev1.EffectiveCodespaceRuntimeSettings,
) (*idleStopResult, error) {
	if runtimeSettings == nil {
		return nil, fmt.Errorf("runtime settings are required")
	}
	request := connect.NewRequest(&codespacev1.RequestIdleStopRequest{
		ProtocolVersion: controlplane.ProtocolVersion,
		RuntimeUuid:     codespaceUUID,
		ObservedSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{
			AutoStopEnabled:       runtimeSettings.GetAutoStopEnabled(),
			IdleTimeoutSeconds:    runtimeSettings.GetIdleTimeoutSeconds(),
			InteractionGeneration: runtimeSettings.GetInteractionGeneration(),
		},
	})
	response, err := a.managerClient().RequestIdleStop(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("request idle stop rpc: %w", err)
	}
	switch {
	case response.Msg.GetPending() != nil:
		return &idleStopResult{
			outcome:           idleStopOutcomePending,
			operationRVersion: response.Msg.GetPending().GetOperationRversion(),
		}, nil
	case response.Msg.GetObservationChanged() != nil:
		return &idleStopResult{
			outcome:         idleStopOutcomeObservationChanged,
			runtimeSettings: response.Msg.GetObservationChanged().GetRuntimeSettings(),
		}, nil
	case response.Msg.GetNotApplicable() != nil:
		return &idleStopResult{
			outcome:       idleStopOutcomeNotApplicable,
			notApplicable: response.Msg.GetNotApplicable().GetReason(),
		}, nil
	default:
		return nil, fmt.Errorf("request idle stop outcome is missing")
	}
}

func (a *Agent) reportBootMetadata(
	ctx context.Context,
	operation *codespacev1.OperationPayload,
	instance *provisioner.Instance,
	stage string,
	startedUnix int64,
) error {
	if instance == nil {
		return fmt.Errorf("runtime instance is required")
	}
	if !IsRuntimeBootStage(stage) {
		return fmt.Errorf("runtime boot stage %q is invalid", stage)
	}
	now := time.Now().Unix()
	if startedUnix <= 0 {
		startedUnix = now
	}
	if now < startedUnix {
		now = startedUnix
	}
	snapshot := RuntimeMetadataSnapshot{
		CodespaceUUID:      operation.GetRuntimeUuid(),
		MetadataGeneration: a.nextRuntimeMetadataGeneration(),
		InstanceName:       instance.Name,
		Workdir:            instance.Workdir,
		Boot: RuntimeMetadataBoot{
			OperationRVersion: operation.GetOperationRversion(),
			Stage:             stage,
			StartedUnix:       startedUnix,
			LastUpdateUnix:    now,
		},
	}
	if a.provisioner != nil {
		if usage, err := a.provisioner.RuntimeResourceUsage(ctx, instance.Name); err == nil {
			snapshot.ResourceUsage = usage
		} else {
			log.Printf("sample runtime resource usage %s: %v", operation.GetRuntimeUuid(), err)
		}
	}
	if a.metadataStateStore != nil {
		if err := a.metadataStateStore.SaveRuntimeMetadataSnapshot(snapshot); err != nil {
			return fmt.Errorf("save runtime metadata snapshot: %w", err)
		}
	}
	if a.metadataPublisher != nil {
		active, err := a.metadataPublisher.ActivateRuntimeMetadata(operation.GetRuntimeUuid())
		if err != nil {
			return fmt.Errorf("activate runtime metadata: %w", err)
		}
		if !active {
			return fmt.Errorf("runtime metadata snapshot is missing after save")
		}
		if stage != RuntimeBootStageReady {
			return nil
		}
		if err := a.metadataPublisher.PublishRuntimeMetadata(ctx, operation.GetRuntimeUuid()); err != nil {
			return fmt.Errorf("publish runtime metadata stage %s: %w", stage, err)
		}
		return nil
	}
	if err := a.publishRuntimeMetadataDirect(ctx, snapshot); err != nil {
		return fmt.Errorf("publish runtime metadata stage %s: %w", stage, err)
	}
	return nil
}

func (a *Agent) publishRuntimeMetadataDirect(ctx context.Context, snapshot RuntimeMetadataSnapshot) error {
	metadata, err := RuntimeMetadataProto(snapshot, nil)
	if err != nil {
		return err
	}
	request := connect.NewRequest(&codespacev1.ReportRuntimeMetadataRequest{
		ProtocolVersion:    controlplane.ProtocolVersion,
		RuntimeUuid:        snapshot.CodespaceUUID,
		MetadataGeneration: snapshot.MetadataGeneration,
		Metadata:           metadata,
	})
	if err := a.checkControlPlaneMessageSize(request.Msg); err != nil {
		return err
	}
	if _, err := a.managerClient().ReportRuntimeMetadata(ctx, request); err != nil {
		return fmt.Errorf("report runtime metadata rpc: %w", err)
	}
	return nil
}

func (a *Agent) nextRuntimeMetadataGeneration() int64 {
	a.metadataMu.Lock()
	defer a.metadataMu.Unlock()

	generation := a.metadataGeneration
	a.metadataGeneration++
	return generation
}

func (a *Agent) finalize(
	ctx context.Context,
	operation *codespacev1.OperationPayload,
	status codespacev1.FinalStatus,
	typ codespacev1.OperationType,
) (finalizeOutcome, error) {
	request := connect.NewRequest(&codespacev1.FinalizeOperationRequest{
		ProtocolVersion:   controlplane.ProtocolVersion,
		RuntimeUuid:       operation.GetRuntimeUuid(),
		OperationRversion: operation.GetOperationRversion(),
		Status:            status,
		OperationType:     typ,
	})
	response, err := a.managerClient().FinalizeOperation(ctx, request)
	if err != nil {
		return finalizeOutcomeAccepted, fmt.Errorf("finalize operation rpc: %w", err)
	}
	if response.Msg.GetResourceAbsent() {
		return finalizeOutcomeResourceAbsent, nil
	}
	return finalizeOutcomeAccepted, nil
}

func (a *Agent) triggerResourceAbsentInventory(ctx context.Context, operation *codespacev1.OperationPayload) {
	if err := a.reportInventoryOnce(ctx); err != nil {
		err = fmt.Errorf("resource absent inventory %s version %d: %w", operation.GetRuntimeUuid(), operation.GetOperationRversion(), err)
		if isManagerCriticalError(err) {
			a.reportCriticalError(err)
			return
		}
		log.Printf("%v", err)
	}
}

func (a *Agent) intervalOrDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (a *Agent) checkControlPlaneMessageSize(message proto.Message) error {
	settings := a.currentServiceSettings()
	return controlplane.CheckMessageSize(message, settings.ControlPlaneMaxMessageSize)
}

func validateDeclareResponse(response *codespacev1.DeclareManagerResponse) (ManagerServiceSettings, error) {
	if response.GetHeartbeatIntervalMilliseconds() <= 0 {
		return ManagerServiceSettings{}, fmt.Errorf("declare response heartbeat interval must be positive")
	}
	if response.GetRuntimeMetadataRefreshIntervalMilliseconds() <= 0 {
		return ManagerServiceSettings{}, fmt.Errorf("declare response runtime metadata refresh interval must be positive")
	}
	if response.GetControlPlaneMaxMessageSizeBytes() <= 0 {
		return ManagerServiceSettings{}, fmt.Errorf("declare response control plane message size must be positive")
	}
	if err := validateDeclareGiteaWebURL(response.GetGiteaWebUrl()); err != nil {
		return ManagerServiceSettings{}, err
	}
	return ManagerServiceSettings{
		HeartbeatInterval:              time.Duration(response.GetHeartbeatIntervalMilliseconds()) * time.Millisecond,
		RuntimeMetadataRefreshInterval: time.Duration(response.GetRuntimeMetadataRefreshIntervalMilliseconds()) * time.Millisecond,
		ControlPlaneMaxMessageSize:     response.GetControlPlaneMaxMessageSizeBytes(),
		GiteaWebURL:                    response.GetGiteaWebUrl(),
	}, nil
}

func validateDeclareGiteaWebURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("declare response gitea web url is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("declare response gitea web url must use http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("declare response gitea web url must include host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("declare response gitea web url must not include userinfo, query, or fragment")
	}
	if parsed.Path == "" || !strings.HasSuffix(parsed.Path, "/") {
		return fmt.Errorf("declare response gitea web url path must end with slash")
	}
	return nil
}

func operationType(operation *codespacev1.OperationPayload) codespacev1.OperationType {
	switch operation.GetCommand().(type) {
	case *codespacev1.OperationPayload_Create:
		return codespacev1.OperationType_OPERATION_TYPE_CREATE
	case *codespacev1.OperationPayload_Resume:
		return codespacev1.OperationType_OPERATION_TYPE_RESUME
	case *codespacev1.OperationPayload_Stop:
		return codespacev1.OperationType_OPERATION_TYPE_STOP
	case *codespacev1.OperationPayload_Delete:
		return codespacev1.OperationType_OPERATION_TYPE_DELETE
	case *codespacev1.OperationPayload_AbortCreate:
		return codespacev1.OperationType_OPERATION_TYPE_CREATE
	case *codespacev1.OperationPayload_AbortResume:
		return codespacev1.OperationType_OPERATION_TYPE_RESUME
	default:
		return codespacev1.OperationType_OPERATION_TYPE_UNSPECIFIED
	}
}

func isDeleteOperation(operation *codespacev1.OperationPayload) bool {
	_, ok := operation.GetCommand().(*codespacev1.OperationPayload_Delete)
	return ok
}

func isStartupOperation(operation *codespacev1.OperationPayload) bool {
	switch operation.GetCommand().(type) {
	case *codespacev1.OperationPayload_Create,
		*codespacev1.OperationPayload_Resume,
		*codespacev1.OperationPayload_AbortCreate,
		*codespacev1.OperationPayload_AbortResume:
		return true
	default:
		return false
	}
}

func isAbortOperation(operation *codespacev1.OperationPayload) bool {
	switch operation.GetCommand().(type) {
	case *codespacev1.OperationPayload_AbortCreate,
		*codespacev1.OperationPayload_AbortResume:
		return true
	default:
		return false
	}
}

func canAbortRunningOperation(current, next *codespacev1.OperationPayload) bool {
	switch current.GetCommand().(type) {
	case *codespacev1.OperationPayload_Create:
		_, ok := next.GetCommand().(*codespacev1.OperationPayload_AbortCreate)
		return ok
	case *codespacev1.OperationPayload_Resume:
		_, ok := next.GetCommand().(*codespacev1.OperationPayload_AbortResume)
		return ok
	default:
		return false
	}
}

func runtimeStateToProto(state provisioner.RuntimeState) codespacev1.RuntimeState {
	switch state {
	case provisioner.RuntimeStateRunning:
		return codespacev1.RuntimeState_RUNTIME_STATE_RUNNING
	case provisioner.RuntimeStateStopped:
		return codespacev1.RuntimeState_RUNTIME_STATE_STOPPED
	case provisioner.RuntimeStateFailed:
		return codespacev1.RuntimeState_RUNTIME_STATE_FAILED
	default:
		return codespacev1.RuntimeState_RUNTIME_STATE_CREATING
	}
}

func runtimeInstanceName(codespaceUUID string) string {
	shortUUID := strings.ReplaceAll(codespaceUUID, "-", "")
	if len(shortUUID) > 20 {
		shortUUID = shortUUID[:20]
	}
	return "cs-" + shortUUID
}

// newCleanupContext bounds detached cleanup by the worker's shutdown deadline.
func (a *Agent) newCleanupContext() (context.Context, context.CancelFunc) {
	parent := a.shutdownContext
	if parent == nil {
		parent = context.Background()
	}
	timeout := a.config.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	if a.config.ExecutionContext != nil && a.config.ExecutionContext.Err() != nil {
		cancel()
	}
	return ctx, cancel
}
