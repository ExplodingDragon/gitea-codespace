// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/provisioner"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

const (
	managerIDHeader     = "x-codespace-manager-id"
	managerSecretHeader = "x-codespace-manager-secret"
	protocolVersion     = 1
	initialReadMaxBytes = 64 * 1024

	gitSSHKeyTypeEd25519 = "ed25519"
	gitSSHKeyTypeRSA4096 = "rsa-4096"

	defaultInventoryInterval        = time.Minute
	maxInventoryInstances           = 10000
	runtimeHealthFailuresBeforeStop = 3

	// RuntimeBootStagePrepareRuntime means the Manager has started preparing the runtime.
	RuntimeBootStagePrepareRuntime = "prepare-runtime"
	// RuntimeBootStageInitializeSystem means the Manager is preparing system credentials.
	RuntimeBootStageInitializeSystem = "initialize-system"
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
	logAuthorizationHeaderPattern = regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic)\s+)[^\s]+`)
	logBearerBasicPattern         = regexp.MustCompile(`(?i)\b((?:bearer|basic)\s+)[A-Za-z0-9._~+/=-]+`)
	logURLUserinfoPattern         = regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
)

var runtimeBootStageRanks = map[string]int{
	RuntimeBootStagePrepareRuntime:   0,
	RuntimeBootStageInitializeSystem: 1,
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
	case RuntimeBootStageInitializeSystem:
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
	BaseURL                     string
	ManagerID                   int64
	ManagerSecret               string
	Name                        string
	GatewayURL                  string
	GatewaySSHAddr              string
	GatewaySSHHostKeyAlgo       string
	GatewaySSHHostKeySHA256     string
	GatewaySSHHostKeyUnix       int64
	Version                     string
	Tags                        []string
	PollInterval                time.Duration
	DeclareInterval             time.Duration
	CapacityTotal               int32
	StartupWorkers              int32
	CleanupWorkers              int32
	HTTPTimeout                 time.Duration
	RuntimeMetadataGeneration   int64
	InventoryGeneration         int64
	Scripts                     provisioner.ScriptSnapshot
	InitialRuntimeGenerations   map[string]int64
	InitialRuntimeTransitions   []RuntimeTransitionSnapshot
	InitialCleanupPendings      []string
	InitialHealthStopPendings   []HealthStopSnapshot
	InitialOperations           []OperationSnapshot
	OperationStateStore         OperationStateStore
	InventoryStateStore         InventoryStateStore
	RuntimeStateStore           RuntimeStateStore
	CleanupStateStore           CleanupStateStore
	HealthStopStateStore        HealthStopStateStore
	ScriptEnvironmentStateStore ScriptEnvironmentStateStore
	RuntimeMetadataStateStore   RuntimeMetadataStateStore
	StartupInputStateStore      StartupInputStateStore
	RuntimeEndpointApplier      RuntimeEndpointApplier
	RuntimeHealthStateStore     RuntimeHealthStateStore
	RuntimeMetadataPublisher    RuntimeMetadataPublisher
	SessionTracker              SessionTracker
	AccessController            AccessController
	ManagerServiceSettings      ManagerServiceSettingsStore
	GitSSHKeyType               string
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
	Scripts     provisioner.ScriptSnapshot
}

// OperationWorkerStage stores the local worker stage for one active operation.
type OperationWorkerStage string

const (
	// OperationWorkerStageActive means the operation has a current local lease and may run.
	OperationWorkerStageActive OperationWorkerStage = "active"
	// OperationWorkerStageLeasePaused means the operation context is retained but local execution is paused.
	OperationWorkerStageLeasePaused OperationWorkerStage = "lease_paused"
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

// ScriptEnvironmentStateStore persists the latest normalized shared script environment.
type ScriptEnvironmentStateStore interface {
	SaveScriptEnvironment(codespaceUUID string, environment map[string]string) error
	LoadScriptEnvironment(codespaceUUID string) (map[string]string, bool, error)
}

// StartupInput stores create-time inputs owned by the Manager after the operation is claimed.
type StartupInput struct {
	CodespaceUUID    string
	UserIdentity     StartupUserIdentity
	RuntimeUserName  string
	EnvironmentTag   string
	RepositoryConfig StartupRepositoryConfig
}

// StartupUserIdentity stores the Gitea user identity used for one-time runtime initialization.
type StartupUserIdentity struct {
	UserID       int64
	Username     string
	DisplayName  string
	GitUserName  string
	GitUserEmail string
}

// StartupRepositoryConfig stores the repository Codespace config fixed for the runtime.
type StartupRepositoryConfig struct {
	Present       bool
	Path          string
	Content       []byte
	SourceRef     string
	ContentSHA256 string
}

// StartupInputStateStore persists create-time startup inputs for resume.
type StartupInputStateStore interface {
	SaveStartupInput(input StartupInput) error
	LoadStartupInput(codespaceUUID string) (StartupInput, bool, error)
}

type memoryStartupInputStore struct {
	mu     sync.Mutex
	inputs map[string]StartupInput
}

func newMemoryStartupInputStore() *memoryStartupInputStore {
	return &memoryStartupInputStore{inputs: map[string]StartupInput{}}
}

func (s *memoryStartupInputStore) SaveStartupInput(input StartupInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(input.CodespaceUUID) == "" {
		return fmt.Errorf("codespace uuid is empty")
	}
	input.RepositoryConfig.Content = append([]byte(nil), input.RepositoryConfig.Content...)
	s.inputs[input.CodespaceUUID] = input
	return nil
}

func (s *memoryStartupInputStore) LoadStartupInput(codespaceUUID string) (StartupInput, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	input, ok := s.inputs[codespaceUUID]
	if !ok {
		return StartupInput{}, false, nil
	}
	input.RepositoryConfig.Content = append([]byte(nil), input.RepositoryConfig.Content...)
	return input, true, nil
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
}

// RuntimeEndpointRoute stores one endpoint route derived from a runtime manifest.
type RuntimeEndpointRoute struct {
	CodespaceUUID  string
	EndpointID     string
	Label          string
	UpstreamScheme string
	UpstreamHost   string
	Public         bool
}

// RuntimeEndpointApplier applies a complete runtime endpoint route set.
type RuntimeEndpointApplier interface {
	ApplyRuntimeEndpointRoutes(codespaceUUID string, routes []RuntimeEndpointRoute) error
}

// RuntimeHealthStateStore loads ready runtime metadata used by health checks.
type RuntimeHealthStateStore interface {
	LoadRuntimeMetadataSnapshot(codespaceUUID string) (RuntimeMetadataSnapshot, bool, error)
}

// RuntimeMetadataPublisher publishes the current complete metadata snapshot.
type RuntimeMetadataPublisher interface {
	NotifyRuntimeMetadata(codespaceUUID string)
	PublishRuntimeMetadata(ctx context.Context, codespaceUUID string) error
}

type runtimeMetadataForgetter interface {
	ForgetRuntimeMetadata(codespaceUUID string)
}

type workspaceGitChecker interface {
	CheckWorkspaceGit(ctx context.Context, instanceName string, workdir string) (provisioner.WorkspaceGitStatus, error)
}

type workspaceAccessChecker interface {
	CheckWorkspaceAccess(ctx context.Context, instanceName string, workdir string) error
}

type operationContext struct {
	operationRVersion int64
	payload           *codespacev1.OperationPayload
	scripts           provisioner.ScriptSnapshot
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
	scriptEnvStateStore  ScriptEnvironmentStateStore
	metadataStateStore   RuntimeMetadataStateStore
	startupInputStore    StartupInputStateStore
	endpointApplier      RuntimeEndpointApplier
	runtimeHealthStore   RuntimeHealthStateStore
	metadataPublisher    RuntimeMetadataPublisher
	sessionTracker       SessionTracker
	accessController     AccessController
	settingsStore        ManagerServiceSettingsStore
	gitSSHKeyType        string
	autoStopMu           sync.Mutex
	autoStops            map[string]*autoStopState
	healthFailures       map[string]int
	healthCandidates     map[string]struct{}
	criticalErrors       chan error
}

// New creates one Manager worker.
func New(config AgentConfig, httpClient *http.Client, provisioner provisioner.Provisioner) *Agent {
	client := newManagerServiceClient(httpClient, config.BaseURL, initialReadMaxBytes)
	metadataGeneration := config.RuntimeMetadataGeneration
	if metadataGeneration <= 0 {
		metadataGeneration = 1
	}
	startupInputStore := config.StartupInputStateStore
	if startupInputStore == nil {
		startupInputStore = newMemoryStartupInputStore()
	}
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
		scriptEnvStateStore:  config.ScriptEnvironmentStateStore,
		metadataStateStore:   config.RuntimeMetadataStateStore,
		startupInputStore:    startupInputStore,
		endpointApplier:      config.RuntimeEndpointApplier,
		runtimeHealthStore:   config.RuntimeHealthStateStore,
		metadataPublisher:    config.RuntimeMetadataPublisher,
		sessionTracker:       config.SessionTracker,
		accessController:     config.AccessController,
		settingsStore:        config.ManagerServiceSettings,
		gitSSHKeyType:        gitSSHKeyType,
		autoStops:            make(map[string]*autoStopState),
		healthFailures:       make(map[string]int),
		healthCandidates:     make(map[string]struct{}),
		criticalErrors:       make(chan error, 1),
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
		codespaceUUID := snapshot.Payload.GetCodespaceUuid()
		operationRVersion := snapshot.Payload.GetOperationRversion()
		if codespaceUUID == "" || operationRVersion <= 0 {
			continue
		}
		agent.activeOperations[codespaceUUID] = &operationContext{
			operationRVersion: operationRVersion,
			payload:           snapshot.Payload,
			scripts:           snapshot.Scripts,
			running:           false,
		}
	}
	return agent
}

func newManagerServiceClient(httpClient connect.HTTPClient, baseURL string, maxBytes int64) codespacev1connect.ManagerServiceClient {
	opts := []connect.ClientOption(nil)
	if maxBytes > 0 {
		max := int(maxBytes)
		if int64(max) != maxBytes {
			max = math.MaxInt
		}
		opts = append(opts, connect.WithReadMaxBytes(max), connect.WithSendMaxBytes(max))
	}
	return codespacev1connect.NewManagerServiceClient(httpClient, baseURL, opts...)
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
		a.client = newManagerServiceClient(a.httpClient, a.baseURL, settings.ControlPlaneMaxMessageSize)
	}
	a.serviceSettings = settings
	a.clientMu.Unlock()
	return nil
}

// Run declares the Manager and processes operations until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
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
	instances, listErr := a.provisioner.ListInstances(ctx)
	capacity := a.fetchCapacity(instances, listErr)
	request := connect.NewRequest(&codespacev1.DeclareManagerRequest{
		ProtocolVersion:                    protocolVersion,
		GatewayUrl:                         a.config.GatewayURL,
		GatewaySshAddr:                     a.config.GatewaySSHAddr,
		Tags:                               append([]string(nil), a.config.Tags...),
		Version:                            a.config.Version,
		Name:                               a.config.Name,
		ManagerRuntimeState:                state,
		GatewaySshHostKeyAlgorithm:         a.config.GatewaySSHHostKeyAlgo,
		GatewaySshHostKeyFingerprintSha256: a.config.GatewaySSHHostKeySHA256,
		GatewaySshHostKeyUpdatedUnix:       a.config.GatewaySSHHostKeyUnix,
		StartupCapacityTotal:               a.config.CapacityTotal,
		StartupCapacityAvailable:           capacity.startup,
	})
	a.setManagerAuth(request.Header())
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
	defer a.releaseFetchReservation(capacity)
	capacity = a.applyStartupAdmission(ctx, capacity)
	request := connect.NewRequest(&codespacev1.FetchOperationsRequest{
		ProtocolVersion:          protocolVersion,
		StartupCapacityAvailable: capacity.startup,
		AcceptedOperationTypes:   capacity.acceptedOperationTypes(),
		MaxNewOperations:         capacity.maxOperations(),
		ObservedOperations:       a.observedOperations(),
		CleanupCapacityAvailable: capacity.cleanup,
	})
	a.setManagerAuth(request.Header())
	response, err := a.managerClient().FetchOperations(ctx, request)
	if err != nil {
		return fmt.Errorf("fetch operations rpc: %w", err)
	}
	for _, operation := range response.Msg.GetOperations() {
		if operation == nil {
			continue
		}
		ok, err := a.validateOperationResponseVersion("fetch operation", operation.GetCodespaceUuid(), requestOperationVersions, operation.GetOperationRversion())
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		duration := operationLeaseDurationFromRequestStart(requestStarted, operation)
		if err := a.startOperation(ctx, operation, duration); err != nil {
			return fmt.Errorf("start operation %s version %d: %w", operation.GetCodespaceUuid(), operation.GetOperationRversion(), err)
		}
	}
	for _, lease := range response.Msg.GetRenewedLeases() {
		if lease == nil {
			continue
		}
		ok, err := a.validateOperationResponseVersion("fetch renewed lease", lease.GetCodespaceUuid(), requestOperationVersions, lease.GetOperationRversion())
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		duration := leaseDurationFromRequestStart(requestStarted, lease.GetLeaseValidForMilliseconds())
		if err := a.resumeRenewedOperation(ctx, lease, duration); err != nil {
			return fmt.Errorf("resume renewed operation %s version %d: %w", lease.GetCodespaceUuid(), lease.GetOperationRversion(), err)
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
		capacity.acceptCreate = false
		capacity.acceptResume = true
		return capacity
	}
	capacity.acceptCreate = admission.CreateAvailable
	capacity.acceptResume = admission.ResumeAvailable
	return capacity
}

func (a *Agent) reportInventoryOnce(ctx context.Context) error {
	instances, err := a.provisioner.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("list runtime instances: %w", err)
	}
	if len(instances) > maxInventoryInstances {
		return fmt.Errorf("runtime inventory has %d instances, limit is %d", len(instances), maxInventoryInstances)
	}
	generation, err := a.nextInventoryGeneration()
	if err != nil {
		return err
	}
	refs := a.runtimeInstanceRefs(instances)
	nextHealthCandidates := runtimeHealthCandidates(refs)
	healthCandidates := a.currentRuntimeHealthCandidates()
	a.updateRuntimeObservations(refs)
	runtimeStates := runtimeStatesByUUID(refs)
	requestOperationVersions := a.currentOperationVersions()
	request := connect.NewRequest(&codespacev1.ReportInstancesRequest{
		ProtocolVersion:     protocolVersion,
		InventoryGeneration: generation,
		Instances:           refs,
	})
	a.setManagerAuth(request.Header())
	response, err := a.managerClient().ReportInstances(ctx, request)
	if err != nil {
		return fmt.Errorf("report instances rpc: %w", err)
	}
	if a.currentInventoryGeneration() != generation {
		return nil
	}
	if err := a.applyInventoryResults(ctx, generation, runtimeStates, requestOperationVersions, healthCandidates, response.Msg.GetResults()); err != nil {
		return err
	}
	a.replaceRuntimeHealthCandidates(nextHealthCandidates)
	return nil
}

func (a *Agent) nextInventoryGeneration() (int64, error) {
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()

	next := a.inventoryGeneration + 1
	if next <= 0 {
		return 0, &categorizedError{
			category: failureLocalStateCommit,
			message:  "inventory_generation exhausted",
		}
	}
	if a.inventoryStore != nil {
		if err := a.inventoryStore.SaveInventoryGeneration(next); err != nil {
			return 0, &categorizedError{
				category: failureLocalStateCommit,
				message:  fmt.Sprintf("save inventory generation %d: %v", next, err),
			}
		}
	}
	a.inventoryGeneration = next
	return next, nil
}

func (a *Agent) currentInventoryGeneration() int64 {
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()

	return a.inventoryGeneration
}

func (a *Agent) runtimeInstanceRefs(instances []*provisioner.Instance) []*codespacev1.RuntimeInstanceRef {
	observed := a.observedOperationVersions()
	refs := make([]*codespacev1.RuntimeInstanceRef, 0, len(instances))
	for _, instance := range instances {
		if instance == nil || instance.CodespaceUUID == "" {
			continue
		}
		refs = append(refs, &codespacev1.RuntimeInstanceRef{
			CodespaceUuid:             instance.CodespaceUUID,
			RuntimeState:              runtimeStateToProto(instance.RuntimeState),
			ObservedOperationRversion: observed[instance.CodespaceUUID],
		})
	}
	return refs
}

func (a *Agent) observedOperationVersions() map[string]int64 {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	observed := make(map[string]int64, len(a.activeOperations))
	for codespaceUUID, operation := range a.activeOperations {
		if operation.payload == nil || operation.operationRVersion <= 0 {
			continue
		}
		observed[codespaceUUID] = operation.operationRVersion
	}
	return observed
}

func (a *Agent) currentOperationVersions() map[string]int64 {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	versions := make(map[string]int64, len(a.activeOperations))
	for codespaceUUID, operation := range a.activeOperations {
		if operation.operationRVersion <= 0 {
			continue
		}
		versions[codespaceUUID] = operation.operationRVersion
	}
	return versions
}

func runtimeStatesByUUID(refs []*codespacev1.RuntimeInstanceRef) map[string]codespacev1.RuntimeState {
	states := make(map[string]codespacev1.RuntimeState, len(refs))
	for _, ref := range refs {
		if ref == nil || ref.GetCodespaceUuid() == "" {
			continue
		}
		states[ref.GetCodespaceUuid()] = ref.GetRuntimeState()
	}
	return states
}

func runtimeHealthCandidates(refs []*codespacev1.RuntimeInstanceRef) map[string]struct{} {
	candidates := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref == nil ||
			ref.GetCodespaceUuid() == "" ||
			ref.GetRuntimeState() != codespacev1.RuntimeState_RUNTIME_STATE_RUNNING {
			continue
		}
		candidates[ref.GetCodespaceUuid()] = struct{}{}
	}
	return candidates
}

func (a *Agent) currentRuntimeHealthCandidates() map[string]struct{} {
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	candidates := make(map[string]struct{}, len(a.healthCandidates))
	for codespaceUUID := range a.healthCandidates {
		candidates[codespaceUUID] = struct{}{}
	}
	return candidates
}

func (a *Agent) replaceRuntimeHealthCandidates(candidates map[string]struct{}) {
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	a.healthCandidates = candidates
}

func (a *Agent) updateRuntimeObservations(refs []*codespacev1.RuntimeInstanceRef) {
	now := time.Now()
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref == nil || ref.GetCodespaceUuid() == "" {
			continue
		}
		codespaceUUID := ref.GetCodespaceUuid()
		seen[codespaceUUID] = struct{}{}
		state := a.autoStopStateLocked(codespaceUUID)
		state.runtimeState = ref.GetRuntimeState()
		if ref.GetRuntimeState() != codespacev1.RuntimeState_RUNTIME_STATE_RUNNING {
			state.idleStarted = time.Time{}
			state.metadataReady = false
		}
		a.refreshIdleStartLocked(codespaceUUID, state, now)
	}
	for codespaceUUID, state := range a.autoStops {
		if _, ok := seen[codespaceUUID]; !ok && state.runtimeState != codespacev1.RuntimeState_RUNTIME_STATE_UNSPECIFIED {
			state.runtimeState = codespacev1.RuntimeState_RUNTIME_STATE_UNSPECIFIED
			state.metadataReady = false
			state.idleStarted = time.Time{}
			state.requestInFlight = false
		}
	}
}

func (a *Agent) applyRuntimeSettings(codespaceUUID string, settings *codespacev1.EffectiveCodespaceRuntimeSettings, now time.Time) {
	if codespaceUUID == "" || settings == nil {
		return
	}
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	state := a.autoStopStateLocked(codespaceUUID)
	oldInteraction := int64(0)
	if state.settings != nil {
		oldInteraction = state.settings.GetInteractionGeneration()
	}
	next := cloneRuntimeSettings(settings)
	if oldInteraction > next.InteractionGeneration {
		next.InteractionGeneration = oldInteraction
	}
	state.settings = next
	state.requestInFlight = false
	if !next.GetAutoStopEnabled() || next.GetIdleTimeoutSeconds() <= 0 {
		state.idleStarted = time.Time{}
		state.retryAfter = time.Time{}
		state.pendingVersion = 0
		return
	}
	if next.GetInteractionGeneration() > oldInteraction {
		state.idleStarted = time.Time{}
		state.retryAfter = time.Time{}
		state.pendingVersion = 0
	}
	a.refreshIdleStartLocked(codespaceUUID, state, now)
}

func (a *Agent) reconcileAutoStops(ctx context.Context) error {
	now := time.Now()
	requests := a.dueAutoStopRequests(now)
	for _, request := range requests {
		result, err := a.requestIdleStop(ctx, request.codespaceUUID, request.settings)
		if err != nil {
			a.finishIdleStopRequest(request.codespaceUUID, now.Add(30*time.Second), 0)
			return err
		}
		a.applyIdleStopResult(request.codespaceUUID, result, now)
	}
	return nil
}

func (a *Agent) dueAutoStopRequests(now time.Time) []autoStopRequest {
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	requests := make([]autoStopRequest, 0)
	for codespaceUUID, state := range a.autoStops {
		if state.requestInFlight || (!state.retryAfter.IsZero() && now.Before(state.retryAfter)) {
			continue
		}
		if !a.autoStopEligibleLocked(codespaceUUID, state) {
			a.refreshIdleStartLocked(codespaceUUID, state, now)
			continue
		}
		if state.idleStarted.IsZero() {
			state.idleStarted = now
			continue
		}
		timeout := time.Duration(state.settings.GetIdleTimeoutSeconds()) * time.Second
		if now.Sub(state.idleStarted) < timeout {
			continue
		}
		state.requestInFlight = true
		requests = append(requests, autoStopRequest{
			codespaceUUID: codespaceUUID,
			settings:      cloneRuntimeSettings(state.settings),
		})
	}
	return requests
}

func (a *Agent) finishIdleStopRequest(codespaceUUID string, retryAfter time.Time, pendingVersion int64) {
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	state := a.autoStops[codespaceUUID]
	if state == nil {
		return
	}
	state.requestInFlight = false
	state.retryAfter = retryAfter
	if pendingVersion > 0 {
		state.pendingVersion = pendingVersion
	}
}

func (a *Agent) applyIdleStopResult(codespaceUUID string, result *idleStopResult, now time.Time) {
	if result == nil {
		a.finishIdleStopRequest(codespaceUUID, now.Add(30*time.Second), 0)
		return
	}
	switch result.outcome {
	case idleStopOutcomePending:
		a.finishIdleStopRequest(codespaceUUID, now.Add(30*time.Second), result.operationRVersion)
	case idleStopOutcomeObservationChanged:
		a.applyRuntimeSettings(codespaceUUID, result.runtimeSettings, now)
	case idleStopOutcomeNotApplicable:
		a.applyIdleStopNotApplicable(codespaceUUID, result.notApplicable, now)
	default:
		a.finishIdleStopRequest(codespaceUUID, now.Add(30*time.Second), 0)
	}
}

func (a *Agent) applyIdleStopNotApplicable(
	codespaceUUID string,
	reason codespacev1.IdleStopNotApplicableReason,
	now time.Time,
) {
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	state := a.autoStops[codespaceUUID]
	if state == nil {
		return
	}
	state.requestInFlight = false
	switch reason {
	case codespacev1.IdleStopNotApplicableReason_IDLE_STOP_NOT_APPLICABLE_REASON_ALREADY_STOPPED:
		state.runtimeState = codespacev1.RuntimeState_RUNTIME_STATE_STOPPED
		state.metadataReady = false
		state.idleStarted = time.Time{}
		state.retryAfter = time.Time{}
	case codespacev1.IdleStopNotApplicableReason_IDLE_STOP_NOT_APPLICABLE_REASON_STATE_UNAVAILABLE:
		state.idleStarted = time.Time{}
		state.retryAfter = time.Time{}
	default:
		state.retryAfter = now.Add(30 * time.Second)
	}
}

func (a *Agent) markRuntimeReady(codespaceUUID string) {
	if codespaceUUID == "" {
		return
	}
	now := time.Now()
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	state := a.autoStopStateLocked(codespaceUUID)
	state.runtimeState = codespacev1.RuntimeState_RUNTIME_STATE_RUNNING
	state.metadataReady = true
	a.refreshIdleStartLocked(codespaceUUID, state, now)
}

func (a *Agent) markRuntimeStopped(codespaceUUID string) {
	a.markRuntimeInactive(codespaceUUID, codespacev1.RuntimeState_RUNTIME_STATE_STOPPED)
}

func (a *Agent) markRuntimeRemoved(codespaceUUID string) {
	if codespaceUUID == "" {
		return
	}
	a.forgetRuntimeMetadata(codespaceUUID)
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	delete(a.autoStops, codespaceUUID)
}

func (a *Agent) forgetRuntimeMetadata(codespaceUUID string) {
	if codespaceUUID == "" {
		return
	}
	if forgetter, ok := a.metadataPublisher.(runtimeMetadataForgetter); ok {
		forgetter.ForgetRuntimeMetadata(codespaceUUID)
	}
}

func (a *Agent) markRuntimeInactive(codespaceUUID string, runtimeState codespacev1.RuntimeState) {
	if codespaceUUID == "" {
		return
	}
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	state := a.autoStopStateLocked(codespaceUUID)
	state.runtimeState = runtimeState
	state.metadataReady = false
	state.idleStarted = time.Time{}
	state.requestInFlight = false
	state.retryAfter = time.Time{}
	state.pendingVersion = 0
}

func (a *Agent) autoStopStateLocked(codespaceUUID string) *autoStopState {
	state := a.autoStops[codespaceUUID]
	if state == nil {
		state = &autoStopState{}
		a.autoStops[codespaceUUID] = state
	}
	return state
}

func (a *Agent) refreshIdleStartLocked(codespaceUUID string, state *autoStopState, now time.Time) {
	if state == nil || !a.autoStopEligibleLocked(codespaceUUID, state) {
		if state != nil && (state.settings == nil || !state.settings.GetAutoStopEnabled() || state.settings.GetIdleTimeoutSeconds() <= 0) {
			state.idleStarted = time.Time{}
		}
		return
	}
	if state.idleStarted.IsZero() {
		state.idleStarted = now
	}
}

func (a *Agent) autoStopEligibleLocked(codespaceUUID string, state *autoStopState) bool {
	if state == nil || state.settings == nil {
		return false
	}
	if state.runtimeState != codespacev1.RuntimeState_RUNTIME_STATE_RUNNING || !state.metadataReady {
		return false
	}
	if !state.settings.GetAutoStopEnabled() || state.settings.GetIdleTimeoutSeconds() <= 0 {
		return false
	}
	if a.liveSessions(codespaceUUID) > 0 {
		return false
	}
	return !a.hasActiveOperation(codespaceUUID)
}

func (a *Agent) hasActiveOperation(codespaceUUID string) bool {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	_, ok := a.activeOperations[codespaceUUID]
	return ok
}

func (a *Agent) liveSessions(codespaceUUID string) int {
	if a.sessionTracker == nil {
		return 0
	}
	return a.sessionTracker.LiveSessions(codespaceUUID)
}

func cloneRuntimeSettings(settings *codespacev1.EffectiveCodespaceRuntimeSettings) *codespacev1.EffectiveCodespaceRuntimeSettings {
	if settings == nil {
		return nil
	}
	return &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       settings.GetAutoStopEnabled(),
		IdleTimeoutSeconds:    settings.GetIdleTimeoutSeconds(),
		InteractionGeneration: settings.GetInteractionGeneration(),
	}
}

func (a *Agent) applyInventoryResults(
	ctx context.Context,
	generation int64,
	runtimeStates map[string]codespacev1.RuntimeState,
	requestOperationVersions map[string]int64,
	healthCandidates map[string]struct{},
	results []*codespacev1.RuntimeInstanceResult,
) error {
	for _, result := range results {
		if result == nil || result.GetCodespaceUuid() == "" {
			continue
		}
		if a.currentInventoryGeneration() != generation {
			return nil
		}
		if err := a.applyInventoryResult(ctx, runtimeStates, requestOperationVersions, healthCandidates, result); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) applyInventoryResult(
	ctx context.Context,
	runtimeStates map[string]codespacev1.RuntimeState,
	requestOperationVersions map[string]int64,
	healthCandidates map[string]struct{},
	result *codespacev1.RuntimeInstanceResult,
) error {
	codespaceUUID := result.GetCodespaceUuid()
	if result.GetRuntimeSettings() != nil {
		a.applyRuntimeSettings(codespaceUUID, result.GetRuntimeSettings(), time.Now())
	}
	switch {
	case result.GetCleanupLocalRuntime() != nil:
		if err := a.saveCleanupPending(codespaceUUID); err != nil {
			return err
		}
		if err := a.clearOperationContext(codespaceUUID, 0); err != nil {
			return err
		}
		if err := a.cleanupLocalRuntime(ctx, codespaceUUID); err != nil {
			return err
		}
	case result.GetStopLocalRuntime() != nil:
		ok, err := a.validateOperationResponseVersion("inventory action", codespaceUUID, requestOperationVersions, result.GetStopLocalRuntime().GetCurrentOperationRversion())
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if !a.operationVersionAtMost(codespaceUUID, result.GetStopLocalRuntime().GetCurrentOperationRversion()) {
			return nil
		}
		if err := a.provisioner.Stop(ctx, runtimeInstanceName(codespaceUUID)); err != nil {
			return fmt.Errorf("stop local runtime %s: %w", codespaceUUID, err)
		}
	case result.GetClearOperationContext() != nil:
		ok, err := a.validateOperationResponseVersion("inventory action", codespaceUUID, requestOperationVersions, result.GetClearOperationContext().GetCurrentOperationRversion())
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := a.clearOperationContext(codespaceUUID, result.GetClearOperationContext().GetCurrentOperationRversion()); err != nil {
			return err
		}
	case result.GetRefetchOperation() != nil:
		ok, err := a.validateOperationResponseVersion("inventory action", codespaceUUID, requestOperationVersions, result.GetRefetchOperation().GetCurrentOperationRversion())
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		log.Printf("inventory requested operation refetch for %s version %d", codespaceUUID, result.GetRefetchOperation().GetCurrentOperationRversion())
	case result.GetReportRuntimeTransition() != nil:
		ok, err := a.validateOperationResponseVersion("inventory action", codespaceUUID, requestOperationVersions, result.GetReportRuntimeTransition().GetCurrentOperationRversion())
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		runtimeState := runtimeStates[codespaceUUID]
		runtimeGeneration, reported, err := a.reportRuntimeTransition(ctx, codespaceUUID, runtimeState, result.GetReportRuntimeTransition().GetCurrentOperationRversion())
		if err != nil {
			return err
		}
		if !reported {
			return nil
		}
		if runtimeState == codespacev1.RuntimeState_RUNTIME_STATE_FAILED {
			if err := a.saveCleanupPending(codespaceUUID); err != nil {
				return err
			}
			if err := a.cleanupLocalRuntime(ctx, codespaceUUID); err != nil {
				return err
			}
			return nil
		}
		if err := a.clearRuntimeTransitionPending(codespaceUUID, runtimeGeneration); err != nil {
			return fmt.Errorf("clear runtime transition pending %s generation %d: %w", codespaceUUID, runtimeGeneration, err)
		}
	default:
		if err := a.repairStableRunningRuntime(ctx, codespaceUUID, runtimeStates, requestOperationVersions, healthCandidates); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) repairStableRunningRuntime(
	ctx context.Context,
	codespaceUUID string,
	runtimeStates map[string]codespacev1.RuntimeState,
	requestOperationVersions map[string]int64,
	healthCandidates map[string]struct{},
) error {
	if runtimeStates[codespaceUUID] != codespacev1.RuntimeState_RUNTIME_STATE_RUNNING || requestOperationVersions[codespaceUUID] != 0 {
		return nil
	}
	if err := a.repairStableRunningCredentials(ctx, codespaceUUID); err != nil {
		return err
	}
	if _, ok := healthCandidates[codespaceUUID]; !ok {
		return nil
	}
	return a.checkStableRunningHealth(ctx, codespaceUUID)
}

func (a *Agent) validateOperationResponseVersion(
	rpc string,
	codespaceUUID string,
	requestOperationVersions map[string]int64,
	responseOperationVersion int64,
) (bool, error) {
	if responseOperationVersion <= 0 {
		return false, &categorizedError{
			category: failureOperationRegression,
			message:  fmt.Sprintf("%s for %s has non-positive operation version %d", rpc, codespaceUUID, responseOperationVersion),
		}
	}
	requestVersion := requestOperationVersions[codespaceUUID]
	localVersion := a.currentOperationVersion(codespaceUUID)
	if responseOperationVersion < requestVersion {
		return false, &categorizedError{
			category: failureOperationRegression,
			message: fmt.Sprintf(
				"%s version regression for %s: request_version=%d local_version=%d response_version=%d",
				rpc,
				codespaceUUID,
				requestVersion,
				localVersion,
				responseOperationVersion,
			),
		}
	}
	if responseOperationVersion < localVersion {
		return false, nil
	}
	return true, nil
}

func (a *Agent) repairStableRunningCredentials(ctx context.Context, codespaceUUID string) error {
	if a.provisioner == nil || codespaceUUID == "" {
		return nil
	}
	instanceName := runtimeInstanceName(codespaceUUID)
	status, err := a.provisioner.CheckCredentials(ctx, instanceName)
	if err != nil {
		return fmt.Errorf("check runtime credentials %s: %w", codespaceUUID, err)
	}
	if !status.GiteaTokenPresent {
		a.closeCodespaceAccess(codespaceUUID)
		observedOperationRVersion, observedErr := a.stableRunningObservedOperationVersion(codespaceUUID)
		if observedErr != nil {
			return observedErr
		}
		if stopErr := a.provisioner.Stop(ctx, instanceName); stopErr != nil {
			return fmt.Errorf("stop runtime with missing gitea token %s: %w", codespaceUUID, stopErr)
		}
		runtimeGeneration, reported, reportErr := a.reportRuntimeTransition(ctx, codespaceUUID, codespacev1.RuntimeState_RUNTIME_STATE_STOPPED, observedOperationRVersion)
		if reportErr != nil {
			return reportErr
		}
		if reported {
			if clearErr := a.clearRuntimeTransitionPending(codespaceUUID, runtimeGeneration); clearErr != nil {
				return fmt.Errorf("clear stopped transition pending %s generation %d: %w", codespaceUUID, runtimeGeneration, clearErr)
			}
		}
		return nil
	}
	if err := a.checkStableRunningWorkspaceGit(ctx, codespaceUUID, instanceName); err != nil {
		a.closeCodespaceAccess(codespaceUUID)
		observedOperationRVersion, observedErr := a.stableRunningObservedOperationVersion(codespaceUUID)
		if observedErr != nil {
			return observedErr
		}
		if stopErr := a.provisioner.Stop(ctx, instanceName); stopErr != nil {
			return fmt.Errorf("stop runtime with invalid workspace git credentials %s: %w", codespaceUUID, stopErr)
		}
		runtimeGeneration, reported, reportErr := a.reportRuntimeTransition(ctx, codespaceUUID, codespacev1.RuntimeState_RUNTIME_STATE_STOPPED, observedOperationRVersion)
		if reportErr != nil {
			return reportErr
		}
		if reported {
			if clearErr := a.clearRuntimeTransitionPending(codespaceUUID, runtimeGeneration); clearErr != nil {
				return fmt.Errorf("clear stopped transition pending %s generation %d: %w", codespaceUUID, runtimeGeneration, clearErr)
			}
		}
		return nil
	}
	if err := a.syncRuntimeEndpointManifest(ctx, codespaceUUID, &provisioner.Instance{
		CodespaceUUID: codespaceUUID,
		Name:          instanceName,
	}); err != nil {
		return fmt.Errorf("sync runtime endpoint manifest %s: %w", codespaceUUID, err)
	}
	return nil
}

func (a *Agent) checkStableRunningWorkspaceGit(ctx context.Context, codespaceUUID string, instanceName string) error {
	checker, ok := a.provisioner.(workspaceGitChecker)
	if !ok || a.scriptEnvStateStore == nil {
		return nil
	}
	environment, ok, err := a.scriptEnvStateStore.LoadScriptEnvironment(codespaceUUID)
	if err != nil {
		return fmt.Errorf("load script environment %s: %w", codespaceUUID, err)
	}
	if !ok {
		return fmt.Errorf("script environment is missing")
	}
	workdir := strings.TrimSpace(environment["CODESPACE_WORKSPACE_DIR"])
	if workdir == "" {
		return fmt.Errorf("workspace path is missing")
	}
	status, err := checker.CheckWorkspaceGit(ctx, instanceName, workdir)
	if err != nil {
		return fmt.Errorf("check workspace git %s: %w", codespaceUUID, err)
	}
	if !status.CredentialConfigured {
		return fmt.Errorf("workspace git credentials are not configured for origin %q", status.OriginURL)
	}
	return nil
}

func (a *Agent) stableRunningObservedOperationVersion(codespaceUUID string) (int64, error) {
	if a.runtimeHealthStore == nil {
		return 0, fmt.Errorf("ready runtime metadata is missing")
	}
	snapshot, ok, err := a.runtimeHealthStore.LoadRuntimeMetadataSnapshot(codespaceUUID)
	if err != nil {
		return 0, fmt.Errorf("load runtime metadata %s: %w", codespaceUUID, err)
	}
	if !ok || snapshot.Boot.Stage != RuntimeBootStageReady || snapshot.Boot.OperationRVersion <= 0 {
		return 0, fmt.Errorf("ready runtime metadata is missing")
	}
	return snapshot.Boot.OperationRVersion, nil
}

func (a *Agent) checkStableRunningHealth(ctx context.Context, codespaceUUID string) error {
	checker, ok := a.provisioner.(workspaceAccessChecker)
	if a.runtimeHealthStore == nil || !ok || codespaceUUID == "" {
		return nil
	}
	snapshot, ok, err := a.runtimeHealthStore.LoadRuntimeMetadataSnapshot(codespaceUUID)
	if err != nil {
		return fmt.Errorf("load runtime metadata for health %s: %w", codespaceUUID, err)
	}
	if !ok || snapshot.Boot.Stage != RuntimeBootStageReady {
		a.clearRuntimeHealthFailure(codespaceUUID)
		return nil
	}
	if err := a.checkRuntimeWorkspaceAccess(ctx, checker, snapshot); err == nil {
		a.clearRuntimeHealthFailure(codespaceUUID)
		return nil
	} else if failures := a.recordRuntimeHealthFailure(codespaceUUID); failures < runtimeHealthFailuresBeforeStop {
		a.closeCodespaceAccess(codespaceUUID)
		log.Printf("runtime health check failed for %s (%d/%d): %v", codespaceUUID, failures, runtimeHealthFailuresBeforeStop, err)
		return nil
	} else {
		log.Printf("runtime health check failed for %s (%d/%d), stopping runtime: %v", codespaceUUID, failures, runtimeHealthFailuresBeforeStop, err)
	}

	pending := HealthStopSnapshot{
		CodespaceUUID:             codespaceUUID,
		ObservedOperationRVersion: snapshot.Boot.OperationRVersion,
	}
	if err := a.saveHealthStopPending(pending); err != nil {
		return err
	}
	runtimeGeneration, reported, err := a.finishHealthStopPending(ctx, pending)
	if err != nil {
		return err
	}
	if reported {
		if err := a.clearRuntimeTransitionPending(codespaceUUID, runtimeGeneration); err != nil {
			return fmt.Errorf("clear unhealthy stopped transition pending %s generation %d: %w", codespaceUUID, runtimeGeneration, err)
		}
	}
	a.clearRuntimeHealthFailure(codespaceUUID)
	return nil
}

func (a *Agent) recordRuntimeHealthFailure(codespaceUUID string) int {
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	a.healthFailures[codespaceUUID]++
	return a.healthFailures[codespaceUUID]
}

func (a *Agent) clearRuntimeHealthFailure(codespaceUUID string) {
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	delete(a.healthFailures, codespaceUUID)
}

func (a *Agent) saveHealthStopPending(pending HealthStopSnapshot) error {
	if pending.CodespaceUUID == "" || pending.ObservedOperationRVersion <= 0 {
		return fmt.Errorf("health stop pending is invalid")
	}
	if a.healthStopStateStore != nil {
		if err := a.healthStopStateStore.SaveHealthStopPending(pending); err != nil {
			return fmt.Errorf("save health stop pending %s: %w", pending.CodespaceUUID, err)
		}
	}
	a.activeMu.Lock()
	a.healthStopPendings[pending.CodespaceUUID] = pending
	a.activeMu.Unlock()
	return nil
}

func (a *Agent) clearHealthStopPendingLocal(codespaceUUID string) {
	a.activeMu.Lock()
	delete(a.healthStopPendings, codespaceUUID)
	a.activeMu.Unlock()
}

func (a *Agent) runHealthStopPendings(ctx context.Context) error {
	a.activeMu.Lock()
	pendings := make([]HealthStopSnapshot, 0, len(a.healthStopPendings))
	for _, pending := range a.healthStopPendings {
		pendings = append(pendings, pending)
	}
	a.activeMu.Unlock()

	for _, pending := range pendings {
		runtimeGeneration, reported, err := a.finishHealthStopPending(ctx, pending)
		if err != nil {
			return err
		}
		if reported {
			if err := a.clearRuntimeTransitionPending(pending.CodespaceUUID, runtimeGeneration); err != nil {
				return fmt.Errorf("clear health stopped transition pending %s generation %d: %w", pending.CodespaceUUID, runtimeGeneration, err)
			}
		}
	}
	return nil
}

func (a *Agent) finishHealthStopPending(ctx context.Context, pending HealthStopSnapshot) (int64, bool, error) {
	a.closeCodespaceAccess(pending.CodespaceUUID)
	instanceName := runtimeInstanceName(pending.CodespaceUUID)
	if err := a.provisioner.Stop(ctx, instanceName); err != nil {
		return 0, false, fmt.Errorf("stop health pending runtime %s: %w", pending.CodespaceUUID, err)
	}
	a.markRuntimeStopped(pending.CodespaceUUID)
	transition, err := a.prepareRuntimeTransitionPending(pending.CodespaceUUID, codespacev1.RuntimeState_RUNTIME_STATE_STOPPED, pending.ObservedOperationRVersion)
	if err != nil {
		return 0, false, err
	}
	a.clearHealthStopPendingLocal(pending.CodespaceUUID)
	if err := a.sendRuntimeTransition(ctx, transition); err != nil {
		return transition.RuntimeGeneration, false, err
	}
	return transition.RuntimeGeneration, true, nil
}

func (a *Agent) cleanupLocalRuntime(ctx context.Context, codespaceUUID string) error {
	if err := a.provisioner.Delete(ctx, runtimeInstanceName(codespaceUUID)); err != nil {
		return fmt.Errorf("cleanup local runtime %s: %w", codespaceUUID, err)
	}
	exists, err := a.runtimeInstanceExists(ctx, codespaceUUID)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("cleanup local runtime %s: runtime instance still exists after delete", codespaceUUID)
	}
	if a.cleanupStateStore != nil {
		if err := a.cleanupStateStore.ClearCodespaceState(codespaceUUID); err != nil {
			return fmt.Errorf("clear codespace cleanup state %s: %w", codespaceUUID, err)
		}
	}
	a.runtimeMu.Lock()
	delete(a.runtimeTransitions, codespaceUUID)
	delete(a.runtimeGenerations, codespaceUUID)
	a.runtimeMu.Unlock()
	a.activeMu.Lock()
	delete(a.cleanupPendings, codespaceUUID)
	a.activeMu.Unlock()
	a.markRuntimeRemoved(codespaceUUID)
	return nil
}

func (a *Agent) runtimeInstanceExists(ctx context.Context, codespaceUUID string) (bool, error) {
	instances, err := a.provisioner.ListInstances(ctx)
	if err != nil {
		return false, fmt.Errorf("confirm runtime cleanup %s: %w", codespaceUUID, err)
	}
	for _, instance := range instances {
		if instance != nil && instance.CodespaceUUID == codespaceUUID {
			return true, nil
		}
	}
	return false, nil
}

func (a *Agent) saveCleanupPending(codespaceUUID string) error {
	if a.cleanupStateStore == nil {
		return nil
	}
	if err := a.cleanupStateStore.SaveCleanupPending(codespaceUUID); err != nil {
		return &categorizedError{
			category: failureLocalStateCommit,
			message:  fmt.Sprintf("save cleanup pending %s: %v", codespaceUUID, err),
		}
	}
	a.activeMu.Lock()
	a.cleanupPendings[codespaceUUID] = struct{}{}
	a.activeMu.Unlock()
	return nil
}

func (a *Agent) clearDeleteCleanupState(codespaceUUID string) error {
	if a.cleanupStateStore != nil {
		if err := a.cleanupStateStore.ClearCodespaceState(codespaceUUID); err != nil {
			return fmt.Errorf("clear delete cleanup state %s: %w", codespaceUUID, err)
		}
	}
	a.runtimeMu.Lock()
	delete(a.runtimeTransitions, codespaceUUID)
	delete(a.runtimeGenerations, codespaceUUID)
	a.runtimeMu.Unlock()
	a.activeMu.Lock()
	delete(a.cleanupPendings, codespaceUUID)
	a.activeMu.Unlock()
	return nil
}

func (a *Agent) runCleanupPendings(ctx context.Context) error {
	a.activeMu.Lock()
	codespaceUUIDs := make([]string, 0, len(a.cleanupPendings))
	for codespaceUUID := range a.cleanupPendings {
		codespaceUUIDs = append(codespaceUUIDs, codespaceUUID)
	}
	a.activeMu.Unlock()

	for _, codespaceUUID := range codespaceUUIDs {
		if err := a.cleanupLocalRuntime(ctx, codespaceUUID); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) reportRuntimeTransition(
	ctx context.Context,
	codespaceUUID string,
	runtimeState codespacev1.RuntimeState,
	observedOperationRVersion int64,
) (int64, bool, error) {
	if runtimeState != codespacev1.RuntimeState_RUNTIME_STATE_STOPPED &&
		runtimeState != codespacev1.RuntimeState_RUNTIME_STATE_FAILED {
		return 0, false, nil
	}
	transition, err := a.prepareRuntimeTransitionPending(codespaceUUID, runtimeState, observedOperationRVersion)
	if err != nil {
		return 0, false, err
	}
	if err := a.sendRuntimeTransition(ctx, transition); err != nil {
		return 0, false, err
	}
	return transition.RuntimeGeneration, true, nil
}

func (a *Agent) sendRuntimeTransition(ctx context.Context, transition RuntimeTransitionSnapshot) error {
	request := connect.NewRequest(&codespacev1.ReportRuntimeTransitionRequest{
		ProtocolVersion:           protocolVersion,
		CodespaceUuid:             transition.CodespaceUUID,
		RuntimeGeneration:         transition.RuntimeGeneration,
		ObservedOperationRversion: transition.ObservedOperationRVersion,
		RuntimeState:              transition.TargetState,
	})
	a.setManagerAuth(request.Header())
	if _, err := a.managerClient().ReportRuntimeTransition(ctx, request); err != nil {
		return fmt.Errorf("report runtime transition rpc: %w", err)
	}
	return nil
}

func (a *Agent) prepareRuntimeTransitionPending(
	codespaceUUID string,
	runtimeState codespacev1.RuntimeState,
	observedOperationRVersion int64,
) (RuntimeTransitionSnapshot, error) {
	a.runtimeMu.Lock()
	if pending, ok := a.runtimeTransitions[codespaceUUID]; ok {
		a.runtimeMu.Unlock()
		return pending, nil
	}
	next := a.runtimeGenerations[codespaceUUID] + 1
	a.runtimeMu.Unlock()

	if next <= 0 {
		return RuntimeTransitionSnapshot{}, fmt.Errorf("runtime_generation exhausted for %s", codespaceUUID)
	}
	transition := RuntimeTransitionSnapshot{
		CodespaceUUID:             codespaceUUID,
		TargetState:               runtimeState,
		RuntimeGeneration:         next,
		ObservedOperationRVersion: observedOperationRVersion,
	}
	if a.runtimeStateStore != nil {
		if err := a.runtimeStateStore.SaveRuntimeTransitionPending(transition); err != nil {
			return RuntimeTransitionSnapshot{}, fmt.Errorf("save runtime transition pending %s generation %d: %w", codespaceUUID, next, err)
		}
	}
	a.runtimeMu.Lock()
	if a.runtimeGenerations[codespaceUUID] < next {
		a.runtimeGenerations[codespaceUUID] = next
	}
	a.runtimeTransitions[codespaceUUID] = transition
	a.runtimeMu.Unlock()
	return transition, nil
}

func (a *Agent) clearRuntimeTransitionPending(codespaceUUID string, runtimeGeneration int64) error {
	if a.runtimeStateStore != nil {
		if err := a.runtimeStateStore.ClearRuntimeTransitionPending(codespaceUUID, runtimeGeneration); err != nil {
			return err
		}
	}
	a.runtimeMu.Lock()
	if pending, ok := a.runtimeTransitions[codespaceUUID]; ok && pending.RuntimeGeneration == runtimeGeneration {
		delete(a.runtimeTransitions, codespaceUUID)
	}
	a.runtimeMu.Unlock()
	return nil
}

func (a *Agent) clearOperationContext(codespaceUUID string, maxOperationRVersion int64) error {
	var operationRVersion int64
	a.activeMu.Lock()
	if current, ok := a.activeOperations[codespaceUUID]; ok {
		operationRVersion = current.operationRVersion
		if maxOperationRVersion == 0 || operationRVersion <= maxOperationRVersion {
			a.stopLeaseLocked(current)
			delete(a.activeOperations, codespaceUUID)
		} else {
			operationRVersion = 0
		}
	}
	a.activeMu.Unlock()

	if operationRVersion > 0 && a.stateStore != nil {
		if err := a.stateStore.DeleteActiveOperation(codespaceUUID, operationRVersion); err != nil {
			return fmt.Errorf("delete operation state %s version %d: %w", codespaceUUID, operationRVersion, err)
		}
	}
	return nil
}

func (a *Agent) operationVersionAtMost(codespaceUUID string, operationRVersion int64) bool {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	current, ok := a.activeOperations[codespaceUUID]
	return !ok || current.operationRVersion <= operationRVersion
}

func (a *Agent) currentOperationVersion(codespaceUUID string) int64 {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	current, ok := a.activeOperations[codespaceUUID]
	if !ok {
		return 0
	}
	return current.operationRVersion
}

type fetchCapacity struct {
	startup      int32
	cleanup      int32
	acceptCreate bool
	acceptResume bool
}

func (c fetchCapacity) acceptedOperationTypes() []codespacev1.AcceptedOperationType {
	if c.startup <= 0 {
		return nil
	}
	types := make([]codespacev1.AcceptedOperationType, 0, 2)
	if c.acceptCreate {
		types = append(types, codespacev1.AcceptedOperationType_ACCEPTED_OPERATION_TYPE_CREATE)
	}
	if c.acceptResume {
		types = append(types, codespacev1.AcceptedOperationType_ACCEPTED_OPERATION_TYPE_RESUME)
	}
	return types
}

func (c fetchCapacity) maxOperations() int32 {
	total := c.startup + c.cleanup
	switch {
	case total <= 0:
		return 1
	case total > 256:
		return 256
	default:
		return total
	}
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
	a.fetchReservedStartup += capacity.startup
	a.fetchReservedCleanup += capacity.cleanup
	return capacity
}

func (a *Agent) releaseFetchReservation(capacity fetchCapacity) {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	a.fetchReservedStartup = positiveInt32(a.fetchReservedStartup - capacity.startup)
	a.fetchReservedCleanup = positiveInt32(a.fetchReservedCleanup - capacity.cleanup)
}

func (a *Agent) fetchCapacityLocked(instances []*provisioner.Instance) fetchCapacity {
	snapshot := a.operationCapacitySnapshotLocked()
	runtimeSlots := positiveInt32(a.runtimeSlotsAvailable(instances, snapshot.startup) - a.fetchReservedStartup)
	startupSlots := positiveInt32(a.startupWorkers() - int32(len(snapshot.startup)) - a.fetchReservedStartup)
	cleanupSlots := positiveInt32(a.cleanupWorkers() - int32(len(snapshot.cleanup)) - int32(len(snapshot.cleanupPendings)) - a.fetchReservedCleanup)
	return fetchCapacity{
		startup:      minInt32(runtimeSlots, startupSlots),
		cleanup:      cleanupSlots,
		acceptCreate: true,
		acceptResume: true,
	}
}

type operationCapacitySnapshot struct {
	startup         map[string]struct{}
	cleanup         map[string]struct{}
	cleanupPendings map[string]struct{}
}

func (a *Agent) operationCapacitySnapshot() operationCapacitySnapshot {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	return a.operationCapacitySnapshotLocked()
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
	return positiveInt32(a.config.CapacityTotal - int32(len(occupied)))
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

func positiveInt32(value int32) int32 {
	if value < 0 {
		return 0
	}
	return value
}

func minInt32(left, right int32) int32 {
	if left < right {
		return left
	}
	return right
}

func (a *Agent) observedOperations() []*codespacev1.ObservedOperation {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	observed := make([]*codespacev1.ObservedOperation, 0, len(a.activeOperations))
	for codespaceUUID, operation := range a.activeOperations {
		if operation.payload == nil || operation.operationRVersion <= 0 {
			continue
		}
		observed = append(observed, &codespacev1.ObservedOperation{
			CodespaceUuid:     codespaceUUID,
			OperationRversion: operation.operationRVersion,
		})
	}
	return observed
}

func (a *Agent) startOperation(ctx context.Context, operation *codespacev1.OperationPayload, leaseDuration time.Duration) error {
	if operation == nil {
		return nil
	}
	codespaceUUID := operation.GetCodespaceUuid()
	operationRVersion := operation.GetOperationRversion()
	if codespaceUUID == "" || operationRVersion <= 0 {
		log.Printf("skip invalid operation %q version %d", codespaceUUID, operationRVersion)
		return nil
	}

	a.activeMu.Lock()
	current, ok := a.activeOperations[codespaceUUID]
	if ok {
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
			scripts := a.config.Scripts
			if a.stateStore != nil {
				if err := a.stateStore.SaveActiveOperation(OperationSnapshot{
					Payload:     operation,
					WorkerStage: OperationWorkerStageActive,
					Scripts:     scripts,
				}); err != nil {
					a.activeMu.Unlock()
					return err
				}
			}
			a.stopLeaseLocked(current)
			operationContext := &operationContext{
				operationRVersion: operationRVersion,
				payload:           operation,
				scripts:           scripts,
				running:           true,
			}
			operationCtx := a.startLeaseLocked(ctx, codespaceUUID, operationContext, leaseDuration)
			a.activeOperations[codespaceUUID] = operationContext
			a.activeMu.Unlock()

			a.closeCodespaceAccess(codespaceUUID)
			a.forgetRuntimeMetadata(codespaceUUID)
			a.runOperation(operationCtx, operation, scripts)
			return nil
		}
		if current.running {
			if canAbortRunningOperation(current.payload, operation) {
				current.payload = operation
				a.stopLeaseLocked(current)
			} else {
				a.activeMu.Unlock()
				return nil
			}
		}
	}
	scripts := a.config.Scripts
	if ok && current.operationRVersion == operationRVersion && current.scripts.Init.Content != "" {
		scripts = current.scripts
	}
	a.activeMu.Unlock()

	if a.stateStore != nil {
		if err := a.stateStore.SaveActiveOperation(OperationSnapshot{
			Payload:     operation,
			WorkerStage: OperationWorkerStageActive,
			Scripts:     scripts,
		}); err != nil {
			return err
		}
	}

	a.activeMu.Lock()
	if current, ok := a.activeOperations[codespaceUUID]; ok {
		if current.operationRVersion == operationRVersion && current.payload == operation {
			current.running = true
			operationCtx := a.startLeaseLocked(ctx, codespaceUUID, current, leaseDuration)
			a.activeMu.Unlock()
			a.runOperation(operationCtx, operation, scripts)
			return nil
		}
		if current.operationRVersion == operationRVersion && !current.running {
			current.payload = operation
			if current.scripts.Init.Content == "" {
				current.scripts = scripts
			}
			current.running = true
			operationCtx := a.startLeaseLocked(ctx, codespaceUUID, current, leaseDuration)
			a.activeMu.Unlock()
			a.runOperation(operationCtx, operation, scripts)
			return nil
		}
		if current.operationRVersion == operationRVersion && canAbortRunningOperation(current.payload, operation) {
			if current.running {
				current.payload = operation
				a.stopLeaseLocked(current)
			}
			if current.scripts.Init.Content == "" {
				current.scripts = scripts
			}
			current.running = true
			operationCtx := a.startLeaseLocked(ctx, codespaceUUID, current, leaseDuration)
			a.activeMu.Unlock()
			a.runOperation(operationCtx, operation, scripts)
			return nil
		}
		a.activeMu.Unlock()
		if current.operationRVersion != operationRVersion {
			log.Printf("skip operation %s version %d while version %d is active", codespaceUUID, operationRVersion, current.operationRVersion)
		}
		return nil
	}
	operationContext := &operationContext{
		operationRVersion: operationRVersion,
		payload:           operation,
		scripts:           scripts,
		running:           true,
	}
	operationCtx := a.startLeaseLocked(ctx, codespaceUUID, operationContext, leaseDuration)
	a.activeOperations[codespaceUUID] = operationContext
	a.activeMu.Unlock()

	a.runOperation(operationCtx, operation, scripts)
	return nil
}

func (a *Agent) resumeRenewedOperation(ctx context.Context, lease *codespacev1.RenewedOperationLease, leaseDuration time.Duration) error {
	if lease == nil || lease.GetCodespaceUuid() == "" || lease.GetOperationRversion() <= 0 {
		return nil
	}
	a.activeMu.Lock()
	current, ok := a.activeOperations[lease.GetCodespaceUuid()]
	if !ok || current.operationRVersion != lease.GetOperationRversion() || current.payload == nil {
		a.activeMu.Unlock()
		return nil
	}
	if current.running {
		a.resetLeaseTimerLocked(lease.GetCodespaceUuid(), current, leaseDuration)
		a.activeMu.Unlock()
		return nil
	}
	current.running = true
	payload := current.payload
	scripts := current.scripts
	operationCtx := a.startLeaseLocked(ctx, lease.GetCodespaceUuid(), current, leaseDuration)
	a.activeMu.Unlock()

	a.runOperation(operationCtx, payload, scripts)
	return nil
}

func (a *Agent) runOperation(ctx context.Context, operation *codespacev1.OperationPayload, scripts provisioner.ScriptSnapshot) {
	codespaceUUID := operation.GetCodespaceUuid()
	operationRVersion := operation.GetOperationRversion()
	go func() {
		if err := a.handleOperation(ctx, operation, scripts); err != nil {
			critical := isManagerCriticalError(err)
			if ctx.Err() != nil && isStartupOperation(operation) {
				if stopErr := a.provisioner.Stop(context.Background(), runtimeInstanceName(codespaceUUID)); stopErr != nil {
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
	var scripts provisioner.ScriptSnapshot
	if ok && current.operationRVersion == operationRVersion && current.payload == operation {
		payload = current.payload
		scripts = current.scripts
	}
	a.activeMu.Unlock()

	if payload != nil && a.stateStore != nil {
		if err := a.stateStore.SaveActiveOperation(OperationSnapshot{
			Payload:     payload,
			WorkerStage: OperationWorkerStageLeasePaused,
			Scripts:     scripts,
		}); err != nil {
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
	if !operation.leaseTimer.Stop() {
		select {
		default:
		}
	}
	operation.leaseTimer.Reset(leaseDuration)
	log.Printf("operation %s version %d local lease renewed", codespaceUUID, operation.operationRVersion)
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

func (a *Agent) handleOperation(ctx context.Context, operation *codespacev1.OperationPayload, scripts provisioner.ScriptSnapshot) error {
	if err := a.updateLog(ctx, operation, "operation started"); err != nil {
		return err
	}

	var err error
	switch command := operation.GetCommand().(type) {
	case *codespacev1.OperationPayload_Create:
		err = a.handleCreate(ctx, operation, command.Create, scripts)
	case *codespacev1.OperationPayload_Resume:
		err = a.handleResume(ctx, operation, command.Resume, scripts)
	case *codespacev1.OperationPayload_Stop:
		err = a.handleStop(ctx, operation, scripts)
	case *codespacev1.OperationPayload_Delete:
		err = a.handleDelete(ctx, operation, true)
	case *codespacev1.OperationPayload_AbortCreate:
		err = a.handleDelete(ctx, operation, false)
	case *codespacev1.OperationPayload_AbortResume:
		err = a.handleStop(ctx, operation, scripts)
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
		if provisioner.IsRecoverableScriptFailure(err) {
			if logErr := a.updateLog(ctx, operation, err.Error()); isManagerCriticalError(logErr) {
				return logErr
			}
			a.closeCodespaceAccess(operation.GetCodespaceUuid())
			return err
		}
		if logErr := a.updateLog(ctx, operation, err.Error()); isManagerCriticalError(logErr) {
			return logErr
		}
		a.closeCodespaceAccess(operation.GetCodespaceUuid())
		finalStatus = codespacev1.FinalStatus_FINAL_STATUS_FAILED
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
		if isDeleteOperation(operation) {
			return a.clearDeleteCleanupState(operation.GetCodespaceUuid())
		}
		return nil
	}
	if isDeleteOperation(operation) {
		return a.clearDeleteCleanupState(operation.GetCodespaceUuid())
	}
	return nil
}

func (a *Agent) handleRuntimeMetadataHardFailure(ctx context.Context, operation *codespacev1.OperationPayload, err error) error {
	codespaceUUID := operation.GetCodespaceUuid()
	if logErr := a.updateLog(ctx, operation, err.Error()); isManagerCriticalError(logErr) {
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
	a.finishOperation(operation.GetCodespaceUuid(), operation.GetOperationRversion(), operation)
	a.triggerResourceAbsentInventory(context.Background(), operation)
}

func (a *Agent) handleCreate(ctx context.Context, operation *codespacev1.OperationPayload, payload *codespacev1.CreateOperationPayload, scripts provisioner.ScriptSnapshot) error {
	startupInput, err := startupInputFromCreatePayload(operation, payload)
	if err != nil {
		return err
	}
	if err := a.saveStartupInput(startupInput); err != nil {
		return err
	}
	instance, err := a.provisioner.CreateOrStart(ctx, provisioner.InstanceSpec{
		CodespaceUUID:  operation.GetCodespaceUuid(),
		Name:           runtimeInstanceName(operation.GetCodespaceUuid()),
		RepoFullName:   payload.GetRepoFullName(),
		EnvironmentTag: payload.GetEnvironmentTag(),
	})
	if err != nil {
		return err
	}
	return a.runStartupOperation(ctx, operation, payload.GetRuntimeSettings(), instance, func(instance *provisioner.Instance, token *codespacev1.RequestGiteaTokenResponse) provisioner.BootstrapRequest {
		return a.createBootstrapRequest(operation, payload, startupInput, instance, token, scripts)
	})
}

func (a *Agent) handleResume(ctx context.Context, operation *codespacev1.OperationPayload, payload *codespacev1.ResumeOperationPayload, scripts provisioner.ScriptSnapshot) error {
	startupInput, ok, err := a.loadStartupInput(operation.GetCodespaceUuid())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("startup input is missing for codespace %s", operation.GetCodespaceUuid())
	}
	instance, err := a.provisioner.StartExisting(ctx, provisioner.InstanceSpec{
		CodespaceUUID: operation.GetCodespaceUuid(),
		Name:          runtimeInstanceName(operation.GetCodespaceUuid()),
	})
	if err != nil {
		return err
	}
	workdir, err := a.loadRuntimeWorkdir(operation.GetCodespaceUuid())
	if err != nil {
		return err
	}
	instance.Workdir = workdir
	return a.runStartupOperation(ctx, operation, payload.GetRuntimeSettings(), instance, func(instance *provisioner.Instance, token *codespacev1.RequestGiteaTokenResponse) provisioner.BootstrapRequest {
		return a.resumeBootstrapRequest(operation, startupInput, instance, token, scripts)
	})
}

func (a *Agent) runStartupOperation(
	ctx context.Context,
	operation *codespacev1.OperationPayload,
	settings *codespacev1.EffectiveCodespaceRuntimeSettings,
	instance *provisioner.Instance,
	newRequest func(instance *provisioner.Instance, token *codespacev1.RequestGiteaTokenResponse) provisioner.BootstrapRequest,
) error {
	codespaceUUID := operation.GetCodespaceUuid()
	a.applyRuntimeSettings(codespaceUUID, settings, time.Now())
	startedUnix := time.Now().Unix()
	logSink := &operationLogSink{agent: a, operation: operation}
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStagePrepareRuntime, startedUnix); err != nil {
		return err
	}
	token, err := a.requestGiteaToken(ctx, codespaceUUID)
	if err != nil {
		return err
	}
	logSink.token = token.GetToken()
	key, err := a.runtimeGitSSHKeySeed(ctx, instance.Name)
	if err != nil {
		return err
	}
	if err := a.provisioner.SeedRuntimeGitSSHKey(ctx, instance.Name, provisioner.RuntimeGitSSHKeySeedRequest{
		GitSSHPrivateKey: key.privateKey,
		GitSSHPublicKey:  key.publicKey,
	}); err != nil {
		return err
	}
	lines, err := a.ensureCodespaceGitSSHKey(ctx, codespaceUUID, key.publicWire)
	if err != nil {
		return err
	}
	if err := a.provisioner.SeedRuntimeCredentials(ctx, instance.Name, provisioner.RuntimeCredentialSeedRequest{
		CodespaceUUID:    codespaceUUID,
		GiteaToken:       token.GetToken(),
		GitSSHPrivateKey: key.privateKey,
		GitSSHPublicKey:  key.publicKey,
		GitSSHKnownHosts: lines,
	}); err != nil {
		return err
	}
	request := newRequest(instance, token)
	request.LogSink = logSink
	if request.Operation == provisioner.ScriptOperationCreate {
		identity, err := a.provisioner.InitializeSystem(ctx, instance.Name, request)
		if err != nil {
			return err
		}
		if err := a.saveScriptEnvironment(codespaceUUID, identity.SharedEnv); err != nil {
			return err
		}
		if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStageInitializeSystem, startedUnix); err != nil {
			return err
		}
		workdir, err := workdirFromSharedEnv(identity.SharedEnv)
		if err != nil {
			return err
		}
		instance.Workdir = workdir
		request = newRequest(instance, token)
		request.LogSink = logSink
	} else {
		if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStageInitializeSystem, startedUnix); err != nil {
			return err
		}
	}
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStagePrepareWorkspace, startedUnix); err != nil {
		return err
	}
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStageStartEnvironment, startedUnix); err != nil {
		return err
	}
	access, err := a.provisioner.StartRuntime(ctx, instance.Name, request)
	if err != nil {
		return err
	}
	if err := a.saveScriptEnvironment(codespaceUUID, access.SharedEnv); err != nil {
		return err
	}
	if err := a.validateRuntimeReady(ctx, codespaceUUID, instance); err != nil {
		return err
	}
	if err := a.syncRuntimeEndpointManifest(ctx, codespaceUUID, instance); err != nil {
		return err
	}
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStagePublishReady, startedUnix); err != nil {
		return err
	}
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStageReady, startedUnix); err != nil {
		return err
	}
	a.markRuntimeReady(codespaceUUID)
	return nil
}

func workdirFromSharedEnv(sharedEnv map[string]string) (string, error) {
	workdir := strings.TrimSpace(sharedEnv["CODESPACE_WORKSPACE_DIR"])
	if !filepath.IsAbs(workdir) {
		return "", fmt.Errorf("CODESPACE_WORKSPACE_DIR must be absolute")
	}
	return workdir, nil
}

func (a *Agent) loadRuntimeWorkdir(codespaceUUID string) (string, error) {
	if a.scriptEnvStateStore == nil {
		return "", fmt.Errorf("script environment store is missing")
	}
	environment, ok, err := a.scriptEnvStateStore.LoadScriptEnvironment(codespaceUUID)
	if err != nil {
		return "", fmt.Errorf("load script environment %s: %w", codespaceUUID, err)
	}
	if !ok {
		return "", fmt.Errorf("script environment is missing for codespace %s", codespaceUUID)
	}
	return workdirFromSharedEnv(environment)
}

func (a *Agent) bootstrapRequest(
	operation *codespacev1.OperationPayload,
	startupInput StartupInput,
	instance *provisioner.Instance,
	token *codespacev1.RequestGiteaTokenResponse,
	scripts provisioner.ScriptSnapshot,
) provisioner.BootstrapRequest {
	return provisioner.BootstrapRequest{
		CodespaceUUID:       operation.GetCodespaceUuid(),
		CodespaceName:       runtimeInstanceName(operation.GetCodespaceUuid()),
		UserID:              startupInput.UserIdentity.UserID,
		UserName:            startupInput.UserIdentity.Username,
		UserDisplayName:     startupInput.UserIdentity.DisplayName,
		GitUserName:         startupInput.UserIdentity.GitUserName,
		GitUserEmail:        startupInput.UserIdentity.GitUserEmail,
		RuntimeUserName:     startupInput.RuntimeUserName,
		GiteaToken:          token.GetToken(),
		ServerURL:           token.GetServerUrl(),
		Workdir:             instance.Workdir,
		EnvironmentTag:      startupInput.EnvironmentTag,
		RepoConfigPresent:   startupInput.RepositoryConfig.Present,
		RepoConfigPath:      startupInput.RepositoryConfig.Path,
		RepoConfigContent:   append([]byte(nil), startupInput.RepositoryConfig.Content...),
		RepoConfigSourceRef: startupInput.RepositoryConfig.SourceRef,
		RepoConfigSHA256:    startupInput.RepositoryConfig.ContentSHA256,
		Scripts:             scripts,
	}
}

func (a *Agent) createBootstrapRequest(
	operation *codespacev1.OperationPayload,
	payload *codespacev1.CreateOperationPayload,
	startupInput StartupInput,
	instance *provisioner.Instance,
	token *codespacev1.RequestGiteaTokenResponse,
	scripts provisioner.ScriptSnapshot,
) provisioner.BootstrapRequest {
	request := a.bootstrapRequest(operation, startupInput, instance, token, scripts)
	request.CodespaceOwnerName = payload.GetCodespaceOwnerName()
	request.RepoCloneHTTPURL = payload.GetRepoCloneHttpUrl()
	request.RepoCloneSSHURL = payload.GetRepoCloneSshUrl()
	request.RepoWebURL = payload.GetRepoWebUrl()
	request.RepoID = payload.GetRepoId()
	request.RepoFullName = payload.GetRepoFullName()
	request.RepoName = payload.GetRepoName()
	request.OwnerID = payload.GetOwnerId()
	request.OwnerName = payload.GetOwnerName()
	request.OwnerType = repositoryOwnerTypeName(payload.GetOwnerType())
	request.OwnerDisplayName = payload.GetOwnerDisplayName()
	request.StartRef = payload.GetStartRef()
	request.RefType = gitRefTypeName(payload.GetRefType())
	request.RefName = payload.GetRefName()
	request.CommitSHA = payload.GetCommitSha()
	request.GitProtocol = gitProtocolName(payload.GetGitProtocol())
	request.Operation = provisioner.ScriptOperationCreate
	return request
}

func (a *Agent) resumeBootstrapRequest(
	operation *codespacev1.OperationPayload,
	startupInput StartupInput,
	instance *provisioner.Instance,
	token *codespacev1.RequestGiteaTokenResponse,
	scripts provisioner.ScriptSnapshot,
) provisioner.BootstrapRequest {
	request := a.bootstrapRequest(operation, startupInput, instance, token, scripts)
	request.Operation = provisioner.ScriptOperationResume
	return request
}

func gitProtocolName(protocol codespacev1.GitProtocol) string {
	switch protocol {
	case codespacev1.GitProtocol_GIT_PROTOCOL_SSH:
		return "ssh"
	default:
		return "http"
	}
}

func repositoryOwnerTypeName(ownerType codespacev1.RepositoryOwnerType) string {
	switch ownerType {
	case codespacev1.RepositoryOwnerType_REPOSITORY_OWNER_TYPE_ORGANIZATION:
		return "organization"
	default:
		return "user"
	}
}

func gitRefTypeName(refType codespacev1.GitRefType) string {
	switch refType {
	case codespacev1.GitRefType_GIT_REF_TYPE_TAG:
		return "tag"
	case codespacev1.GitRefType_GIT_REF_TYPE_COMMIT:
		return "commit"
	default:
		return "branch"
	}
}

func startupInputFromCreatePayload(operation *codespacev1.OperationPayload, payload *codespacev1.CreateOperationPayload) (StartupInput, error) {
	if operation == nil {
		return StartupInput{}, fmt.Errorf("operation is required")
	}
	if payload == nil {
		return StartupInput{}, fmt.Errorf("create payload is required")
	}
	identity := payload.GetUserIdentity()
	if identity == nil {
		return StartupInput{}, fmt.Errorf("create user identity is required")
	}
	username := strings.TrimSpace(identity.GetUsername())
	gitUserName := strings.TrimSpace(identity.GetGitUserName())
	gitUserEmail := strings.TrimSpace(identity.GetGitUserEmail())
	if username == "" {
		return StartupInput{}, fmt.Errorf("create user identity username is required")
	}
	if gitUserName == "" {
		return StartupInput{}, fmt.Errorf("create git user name is required")
	}
	if gitUserEmail == "" {
		return StartupInput{}, fmt.Errorf("create git user email is required")
	}
	environmentTag := strings.TrimSpace(payload.GetEnvironmentTag())
	if environmentTag == "" {
		return StartupInput{}, fmt.Errorf("create environment tag is required")
	}
	runtimeUserName := deriveRuntimeUserName(username)
	repoConfig := payload.GetRepositoryConfig()
	startupInput := StartupInput{
		CodespaceUUID:   operation.GetCodespaceUuid(),
		RuntimeUserName: runtimeUserName,
		EnvironmentTag:  environmentTag,
		UserIdentity: StartupUserIdentity{
			UserID:       identity.GetUserId(),
			Username:     username,
			DisplayName:  strings.TrimSpace(identity.GetDisplayName()),
			GitUserName:  gitUserName,
			GitUserEmail: gitUserEmail,
		},
	}
	if repoConfig != nil {
		startupInput.RepositoryConfig = StartupRepositoryConfig{
			Present:       repoConfig.GetPresent(),
			Path:          strings.TrimSpace(repoConfig.GetPath()),
			Content:       append([]byte(nil), repoConfig.GetContent()...),
			SourceRef:     strings.TrimSpace(repoConfig.GetSourceRef()),
			ContentSHA256: strings.TrimSpace(repoConfig.GetContentSha256()),
		}
	}
	return startupInput, nil
}

func deriveRuntimeUserName(username string) string {
	username = strings.ToLower(strings.TrimSpace(username))
	var builder strings.Builder
	lastSeparator := false
	for _, r := range username {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastSeparator = false
		case r == '_' || r == '-':
			if !lastSeparator {
				builder.WriteByte('-')
				lastSeparator = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			continue
		default:
			if !lastSeparator {
				builder.WriteByte('-')
				lastSeparator = true
			}
		}
	}
	value := strings.Trim(builder.String(), "-_")
	if value == "" {
		value = "codespace"
	}
	if value[0] >= '0' && value[0] <= '9' {
		value = "u-" + value
	}
	if isReservedRuntimeUserName(value) {
		value = "u-" + value
	}
	if len(value) > 32 {
		value = strings.Trim(value[:32], "-_")
	}
	if value == "" {
		return "codespace"
	}
	return value
}

func isReservedRuntimeUserName(username string) bool {
	switch username {
	case "root", "daemon", "bin", "sys", "sync", "games", "man", "lp", "mail", "news", "uucp", "proxy", "www-data", "backup", "list", "irc", "gnats", "nobody":
		return true
	default:
		return false
	}
}

func (a *Agent) saveStartupInput(input StartupInput) error {
	if a.startupInputStore == nil {
		return nil
	}
	if err := a.startupInputStore.SaveStartupInput(input); err != nil {
		return fmt.Errorf("save startup input: %w", err)
	}
	return nil
}

func (a *Agent) loadStartupInput(codespaceUUID string) (StartupInput, bool, error) {
	if a.startupInputStore == nil {
		return StartupInput{}, false, nil
	}
	input, ok, err := a.startupInputStore.LoadStartupInput(codespaceUUID)
	if err != nil {
		return StartupInput{}, false, fmt.Errorf("load startup input: %w", err)
	}
	return input, ok, nil
}

type runtimeGitSSHKeySeed struct {
	privateKey []byte
	publicKey  []byte
	publicWire []byte
}

func generateRuntimeGitSSHKey(keyType string) (runtimeGitSSHKeySeed, error) {
	switch normalizeRuntimeGitSSHKeyType(keyType) {
	case gitSSHKeyTypeRSA4096:
		return generateRSARuntimeGitSSHKey()
	default:
		return generateEd25519RuntimeGitSSHKey()
	}
}

func (a *Agent) runtimeGitSSHKeySeed(ctx context.Context, instanceName string) (runtimeGitSSHKeySeed, error) {
	status, err := a.provisioner.CheckCredentials(ctx, instanceName)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("check runtime git ssh key: %w", err)
	}
	if len(bytes.TrimSpace(status.GitSSHPrivateKey)) == 0 && len(bytes.TrimSpace(status.GitSSHPublicKey)) == 0 {
		return generateRuntimeGitSSHKey(a.gitSSHKeyType)
	}
	return runtimeGitSSHKeySeedFromCredentials(status.GitSSHPrivateKey, status.GitSSHPublicKey)
}

func runtimeGitSSHKeySeedFromCredentials(privateKeyPEM, publicKeyAuthorized []byte) (runtimeGitSSHKeySeed, error) {
	privateKeyPEM = bytes.TrimSpace(privateKeyPEM)
	publicKeyAuthorized = bytes.TrimSpace(publicKeyAuthorized)
	if len(privateKeyPEM) == 0 || len(publicKeyAuthorized) == 0 {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("runtime git ssh private and public key files must both exist")
	}
	rawPrivateKey, err := ssh.ParseRawPrivateKey(privateKeyPEM)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("parse runtime git ssh private key: %w", err)
	}
	signer, ok := rawPrivateKey.(crypto.Signer)
	if !ok {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("runtime git ssh private key is not a signer")
	}
	privatePublicKey, err := ssh.NewPublicKey(signer.Public())
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh private key public half: %w", err)
	}
	publicKey, _, _, _, err := ssh.ParseAuthorizedKey(publicKeyAuthorized)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("parse runtime git ssh public key: %w", err)
	}
	if !bytes.Equal(privatePublicKey.Marshal(), publicKey.Marshal()) {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("runtime git ssh private and public key do not match")
	}
	privateKeyCopy := append([]byte(nil), privateKeyPEM...)
	privateKeyCopy = append(privateKeyCopy, '\n')
	return runtimeGitSSHKeySeed{
		privateKey: privateKeyCopy,
		publicKey:  ssh.MarshalAuthorizedKey(publicKey),
		publicWire: publicKey.Marshal(),
	}, nil
}

func generateEd25519RuntimeGitSSHKey() (runtimeGitSSHKeySeed, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("generate runtime git ssh key: %w", err)
	}
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh public key: %w", err)
	}
	privateBlock, err := ssh.MarshalPrivateKey(privateKey, "gitea-codespace")
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh private key: %w", err)
	}
	return runtimeGitSSHKeySeed{
		privateKey: pem.EncodeToMemory(privateBlock),
		publicKey:  ssh.MarshalAuthorizedKey(sshPublicKey),
		publicWire: sshPublicKey.Marshal(),
	}, nil
}

func generateRSARuntimeGitSSHKey() (runtimeGitSSHKeySeed, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("generate runtime git ssh key: %w", err)
	}
	sshPublicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh public key: %w", err)
	}
	privateBlock, err := ssh.MarshalPrivateKey(privateKey, "gitea-codespace")
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh private key: %w", err)
	}
	return runtimeGitSSHKeySeed{
		privateKey: pem.EncodeToMemory(privateBlock),
		publicKey:  ssh.MarshalAuthorizedKey(sshPublicKey),
		publicWire: sshPublicKey.Marshal(),
	}, nil
}

func normalizeRuntimeGitSSHKeyType(keyType string) string {
	switch strings.ToLower(strings.TrimSpace(keyType)) {
	case "", gitSSHKeyTypeEd25519:
		return gitSSHKeyTypeEd25519
	case gitSSHKeyTypeRSA4096:
		return gitSSHKeyTypeRSA4096
	default:
		return gitSSHKeyTypeEd25519
	}
}

func (a *Agent) ensureCodespaceGitSSHKey(ctx context.Context, codespaceUUID string, publicKey []byte) ([]string, error) {
	request := connect.NewRequest(&codespacev1.EnsureCodespaceGitSSHKeyRequest{
		ProtocolVersion: protocolVersion,
		CodespaceUuid:   codespaceUUID,
		PublicKey:       publicKey,
	})
	a.setManagerAuth(request.Header())
	response, err := a.managerClient().EnsureCodespaceGitSSHKey(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("ensure codespace git ssh key rpc: %w", err)
	}
	return append([]string(nil), response.Msg.GetKnownHostsLines()...), nil
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
	if len(declarations) == 0 {
		if err := a.endpointApplier.ApplyRuntimeEndpointRoutes(codespaceUUID, nil); err != nil {
			return fmt.Errorf("apply runtime endpoint routes: %w", err)
		}
		return nil
	}
	host, err := a.runtimeEndpointHost(ctx, codespaceUUID, instance)
	if err != nil {
		return err
	}
	routes := make([]RuntimeEndpointRoute, 0, len(declarations))
	for _, declaration := range declarations {
		if declaration.UpstreamPort < 1 || declaration.UpstreamPort > 65535 {
			return fmt.Errorf("endpoint %s upstream_port is invalid", declaration.EndpointID)
		}
		routes = append(routes, RuntimeEndpointRoute{
			CodespaceUUID:  codespaceUUID,
			EndpointID:     declaration.EndpointID,
			Label:          declaration.Label,
			UpstreamScheme: declaration.UpstreamScheme,
			UpstreamHost:   net.JoinHostPort(host, fmt.Sprintf("%d", declaration.UpstreamPort)),
			Public:         declaration.Public,
		})
	}
	if err := a.endpointApplier.ApplyRuntimeEndpointRoutes(codespaceUUID, routes); err != nil {
		return fmt.Errorf("apply runtime endpoint routes: %w", err)
	}
	return nil
}

func (a *Agent) runtimeEndpointHost(ctx context.Context, codespaceUUID string, instance *provisioner.Instance) (string, error) {
	host := strings.TrimSpace(instance.CommunicationHost)
	if host != "" {
		return host, nil
	}
	instances, err := a.provisioner.ListInstances(ctx)
	if err != nil {
		return "", fmt.Errorf("list instances for endpoint host: %w", err)
	}
	for _, candidate := range instances {
		if candidate == nil || candidate.CodespaceUUID != codespaceUUID {
			continue
		}
		host = strings.TrimSpace(candidate.CommunicationHost)
		if host != "" {
			return host, nil
		}
	}
	return "", fmt.Errorf("runtime communication host is unavailable")
}

func (a *Agent) saveScriptEnvironment(codespaceUUID string, environment map[string]string) error {
	if a.scriptEnvStateStore == nil {
		return nil
	}
	if err := a.scriptEnvStateStore.SaveScriptEnvironment(codespaceUUID, environment); err != nil {
		return &categorizedError{
			category: failureLocalStateCommit,
			message:  fmt.Sprintf("save script environment %s: %v", codespaceUUID, err),
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
	if !ok {
		return nil
	}
	status, err := checker.CheckWorkspaceGit(ctx, instance.Name, instance.Workdir)
	if err != nil {
		return fmt.Errorf("check workspace git %s: %w", codespaceUUID, err)
	}
	if !status.CredentialConfigured {
		return fmt.Errorf("workspace git credentials are not configured for origin %q", status.OriginURL)
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

func (a *Agent) handleStop(ctx context.Context, operation *codespacev1.OperationPayload, scripts provisioner.ScriptSnapshot) error {
	a.closeCodespaceAccess(operation.GetCodespaceUuid())
	stopRequest := provisioner.BootstrapRequest{
		CodespaceUUID: operation.GetCodespaceUuid(),
		CodespaceName: runtimeInstanceName(operation.GetCodespaceUuid()),
		Operation:     provisioner.ScriptOperationStop,
		Scripts:       scripts,
		LogSink:       &operationLogSink{agent: a, operation: operation},
	}
	if workdir, err := a.loadRuntimeWorkdir(operation.GetCodespaceUuid()); err == nil {
		stopRequest.Workdir = workdir
	} else {
		log.Printf("skip stop script workspace context for %s: %v", operation.GetCodespaceUuid(), err)
	}
	if access, err := a.provisioner.StopRuntime(ctx, runtimeInstanceName(operation.GetCodespaceUuid()), stopRequest); err != nil {
		log.Printf("codespace stop script failed for %s: %v", operation.GetCodespaceUuid(), err)
	} else if err := a.saveScriptEnvironment(operation.GetCodespaceUuid(), access.SharedEnv); err != nil {
		return err
	}
	if err := a.provisioner.Stop(ctx, runtimeInstanceName(operation.GetCodespaceUuid())); err != nil {
		return err
	}
	a.markRuntimeStopped(operation.GetCodespaceUuid())
	return nil
}

func (a *Agent) handleDelete(ctx context.Context, operation *codespacev1.OperationPayload, cleanupPending bool) error {
	a.closeCodespaceAccess(operation.GetCodespaceUuid())
	if cleanupPending {
		if err := a.saveCleanupPending(operation.GetCodespaceUuid()); err != nil {
			return err
		}
	}
	if err := a.provisioner.Delete(ctx, runtimeInstanceName(operation.GetCodespaceUuid())); err != nil {
		return err
	}
	a.markRuntimeRemoved(operation.GetCodespaceUuid())
	return nil
}

func (a *Agent) closeCodespaceAccess(codespaceUUID string) {
	if a.accessController == nil || codespaceUUID == "" {
		return
	}
	a.accessController.CloseCodespaceAccess(codespaceUUID)
}

func (a *Agent) requestGiteaToken(ctx context.Context, codespaceUUID string) (*codespacev1.RequestGiteaTokenResponse, error) {
	request := connect.NewRequest(&codespacev1.RequestGiteaTokenRequest{
		ProtocolVersion: protocolVersion,
		CodespaceUuid:   codespaceUUID,
	})
	a.setManagerAuth(request.Header())
	response, err := a.managerClient().RequestGiteaToken(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("request gitea token rpc: %w", err)
	}
	return response.Msg, nil
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
		ProtocolVersion:               protocolVersion,
		CodespaceUuid:                 codespaceUUID,
		ObservedAutoStopEnabled:       runtimeSettings.GetAutoStopEnabled(),
		ObservedIdleTimeoutSeconds:    runtimeSettings.GetIdleTimeoutSeconds(),
		ObservedInteractionGeneration: runtimeSettings.GetInteractionGeneration(),
	})
	a.setManagerAuth(request.Header())
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
		CodespaceUUID:      operation.GetCodespaceUuid(),
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
			log.Printf("sample runtime resource usage %s: %v", operation.GetCodespaceUuid(), err)
		}
	}
	if a.metadataStateStore != nil {
		if err := a.metadataStateStore.SaveRuntimeMetadataSnapshot(snapshot); err != nil {
			return fmt.Errorf("save runtime metadata snapshot: %w", err)
		}
	}
	if a.metadataPublisher != nil {
		if stage != RuntimeBootStageReady {
			a.metadataPublisher.NotifyRuntimeMetadata(operation.GetCodespaceUuid())
			return nil
		}
		if err := a.metadataPublisher.PublishRuntimeMetadata(ctx, operation.GetCodespaceUuid()); err != nil {
			return fmt.Errorf("publish runtime metadata stage %s: %w", stage, err)
		}
		return nil
	}
	if err := a.publishRuntimeMetadataDirect(ctx, snapshot); err != nil {
		return fmt.Errorf("publish runtime metadata stage %s: %w", stage, err)
	}
	return nil
}

func (a *Agent) reportReadyMetadata(ctx context.Context, operation *codespacev1.OperationPayload, instance *provisioner.Instance) error {
	return a.reportBootMetadata(ctx, operation, instance, RuntimeBootStageReady, time.Now().Unix())
}

func (a *Agent) publishRuntimeMetadataDirect(ctx context.Context, snapshot RuntimeMetadataSnapshot) error {
	metadata, err := RuntimeMetadataProto(snapshot, nil)
	if err != nil {
		return err
	}
	request := connect.NewRequest(&codespacev1.ReportRuntimeMetadataRequest{
		ProtocolVersion:    protocolVersion,
		CodespaceUuid:      snapshot.CodespaceUUID,
		MetadataGeneration: snapshot.MetadataGeneration,
		Metadata:           metadata,
	})
	a.setManagerAuth(request.Header())
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

func (a *Agent) updateLog(ctx context.Context, operation *codespacev1.OperationPayload, message string) error {
	return a.updateLogLines(ctx, operation, []*codespacev1.LogLine{{
		TimestampUnixNano: time.Now().UnixNano(),
		Message:           a.redactLogMessage(message, ""),
	}})
}

type operationLogSink struct {
	agent     *Agent
	operation *codespacev1.OperationPayload
	token     string
	mu        sync.Mutex
}

func (s *operationLogSink) WriteLifecycleLog(ctx context.Context, message string) error {
	if s == nil || s.agent == nil || s.operation == nil || message == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agent.updateLog(ctx, s.operation, s.agent.redactLogMessage(message, s.token))
}

func (a *Agent) redactLogMessage(message, operationToken string) string {
	for _, secret := range []string{a.config.ManagerSecret, operationToken} {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	message = logAuthorizationHeaderPattern.ReplaceAllString(message, "${1}[redacted]")
	message = logBearerBasicPattern.ReplaceAllString(message, "${1}[redacted]")
	message = logURLUserinfoPattern.ReplaceAllString(message, "${1}[redacted]@")
	return message
}

func (a *Agent) updateLogLines(ctx context.Context, operation *codespacev1.OperationPayload, lines []*codespacev1.LogLine) error {
	offset := operation.GetLogOffset()
	for len(lines) > 0 {
		count, err := a.updateLogBatchSize(operation, offset, lines)
		if err != nil {
			return err
		}
		request := connect.NewRequest(&codespacev1.UpdateLogRequest{
			ProtocolVersion:   protocolVersion,
			CodespaceUuid:     operation.GetCodespaceUuid(),
			OperationRversion: operation.GetOperationRversion(),
			Offset:            offset,
			Lines:             lines[:count],
		})
		a.setManagerAuth(request.Header())
		response, err := a.managerClient().UpdateLog(ctx, request)
		if err != nil {
			return fmt.Errorf("update log rpc: %w", err)
		}
		offset = response.Msg.GetNextOffset()
		operation.LogOffset = offset
		lines = lines[count:]
	}
	return nil
}

func (a *Agent) updateLogBatchSize(operation *codespacev1.OperationPayload, offset int64, lines []*codespacev1.LogLine) (int, error) {
	if len(lines) == 0 {
		return 0, nil
	}
	request := connect.NewRequest(&codespacev1.UpdateLogRequest{
		ProtocolVersion:   protocolVersion,
		CodespaceUuid:     operation.GetCodespaceUuid(),
		OperationRversion: operation.GetOperationRversion(),
		Offset:            offset,
		Lines:             lines[:1],
	})
	if err := a.checkControlPlaneMessageSize(request.Msg); err != nil {
		return 0, err
	}
	for count := 2; count <= len(lines); count++ {
		request.Msg.Lines = lines[:count]
		if err := a.checkControlPlaneMessageSize(request.Msg); err != nil {
			return count - 1, nil
		}
	}
	return len(lines), nil
}

func (a *Agent) finalize(
	ctx context.Context,
	operation *codespacev1.OperationPayload,
	status codespacev1.FinalStatus,
	typ codespacev1.OperationType,
) (finalizeOutcome, error) {
	request := connect.NewRequest(&codespacev1.FinalizeOperationRequest{
		ProtocolVersion:   protocolVersion,
		CodespaceUuid:     operation.GetCodespaceUuid(),
		OperationRversion: operation.GetOperationRversion(),
		Final: &codespacev1.FinalResult{
			Status:        status,
			OperationType: typ,
		},
	})
	a.setManagerAuth(request.Header())
	response, err := a.managerClient().FinalizeOperation(ctx, request)
	if err != nil {
		return finalizeOutcomeAccepted, fmt.Errorf("finalize operation rpc: %w", err)
	}
	if response.Msg.GetFinalAccepted() != nil ||
		response.Msg.GetIdempotentDone() != nil ||
		response.Msg.GetStaleOperation() != nil {
		return finalizeOutcomeAccepted, nil
	}
	if response.Msg.GetResourceAbsent() != nil {
		return finalizeOutcomeResourceAbsent, nil
	}
	return finalizeOutcomeAccepted, fmt.Errorf("finalize operation outcome is missing")
}

func (a *Agent) triggerResourceAbsentInventory(ctx context.Context, operation *codespacev1.OperationPayload) {
	if err := a.reportInventoryOnce(ctx); err != nil {
		err = fmt.Errorf("resource absent inventory %s version %d: %w", operation.GetCodespaceUuid(), operation.GetOperationRversion(), err)
		if isManagerCriticalError(err) {
			a.reportCriticalError(err)
			return
		}
		log.Printf("%v", err)
	}
}

func (a *Agent) setManagerAuth(header http.Header) {
	header.Set(managerIDHeader, fmt.Sprintf("%d", a.config.ManagerID))
	header.Set(managerSecretHeader, a.config.ManagerSecret)
}

func (a *Agent) intervalOrDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (a *Agent) checkControlPlaneMessageSize(message proto.Message) error {
	settings := a.currentServiceSettings()
	maxSize := settings.ControlPlaneMaxMessageSize
	if maxSize <= 0 || message == nil {
		return nil
	}
	size := proto.Size(message)
	if int64(size) <= maxSize {
		return nil
	}
	return fmt.Errorf("control plane message size %d exceeds limit %d", size, maxSize)
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
