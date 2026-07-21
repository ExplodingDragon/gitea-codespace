// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/provisioner"
)

const (
	managerIDHeader     = "x-codespace-manager-id"
	managerSecretHeader = "x-codespace-manager-secret"
	protocolVersion     = 1

	defaultInventoryInterval = time.Minute
	maxInventoryInstances    = 10000
)

// AgentConfig configures the Manager worker.
type AgentConfig struct {
	BaseURL                   string
	ManagerID                 int64
	ManagerSecret             string
	Name                      string
	GatewayURL                string
	GatewaySSHAddr            string
	GatewaySSHHostKeyAlgo     string
	GatewaySSHHostKeySHA256   string
	GatewaySSHHostKeyUnix     int64
	Version                   string
	Tags                      []string
	PollInterval              time.Duration
	DeclareInterval           time.Duration
	CapacityTotal             int32
	CapacityAvailable         int32
	CleanupCapacityAvailable  int32
	MaxOperations             int32
	HTTPTimeout               time.Duration
	RuntimeMetadataGeneration int64
	InventoryGeneration       int64
	InitialRuntimeGenerations map[string]int64
	InitialRuntimeTransitions []RuntimeTransitionSnapshot
	InitialCleanupPendings    []string
	InitialOperations         []OperationSnapshot
	OperationStateStore       OperationStateStore
	InventoryStateStore       InventoryStateStore
	RuntimeStateStore         RuntimeStateStore
	CleanupStateStore         CleanupStateStore
	SessionTracker            SessionTracker
	ManagerServiceSettings    ManagerServiceSettingsStore
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

type operationContext struct {
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
	config              AgentConfig
	client              codespacev1connect.ManagerServiceClient
	provisioner         provisioner.Provisioner
	metadataGeneration  int64
	metadataMu          sync.Mutex
	inventoryGeneration int64
	inventoryMu         sync.Mutex
	runtimeMu           sync.Mutex
	runtimeGenerations  map[string]int64
	runtimeTransitions  map[string]RuntimeTransitionSnapshot
	cleanupPendings     map[string]struct{}
	activeMu            sync.Mutex
	activeOperations    map[string]*operationContext
	stateStore          OperationStateStore
	inventoryStore      InventoryStateStore
	runtimeStateStore   RuntimeStateStore
	cleanupStateStore   CleanupStateStore
	sessionTracker      SessionTracker
	settingsStore       ManagerServiceSettingsStore
	autoStopMu          sync.Mutex
	autoStops           map[string]*autoStopState
	criticalErrors      chan error
}

// New creates one Manager worker.
func New(config AgentConfig, httpClient *http.Client, provisioner provisioner.Provisioner) *Agent {
	client := codespacev1connect.NewManagerServiceClient(httpClient, config.BaseURL)
	metadataGeneration := config.RuntimeMetadataGeneration
	if metadataGeneration <= 0 {
		metadataGeneration = 1
	}
	agent := &Agent{
		config:              config,
		client:              client,
		provisioner:         provisioner,
		metadataGeneration:  metadataGeneration,
		inventoryGeneration: config.InventoryGeneration,
		runtimeGenerations:  make(map[string]int64),
		runtimeTransitions:  make(map[string]RuntimeTransitionSnapshot),
		cleanupPendings:     make(map[string]struct{}),
		activeOperations:    make(map[string]*operationContext),
		stateStore:          config.OperationStateStore,
		inventoryStore:      config.InventoryStateStore,
		runtimeStateStore:   config.RuntimeStateStore,
		cleanupStateStore:   config.CleanupStateStore,
		sessionTracker:      config.SessionTracker,
		settingsStore:       config.ManagerServiceSettings,
		autoStops:           make(map[string]*autoStopState),
		criticalErrors:      make(chan error, 1),
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
			running:           false,
		}
	}
	return agent
}

// Run declares the Manager and processes operations until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.runCleanupPendings(ctx); err != nil {
		return runContextError(err)
	}
	if err := a.declareUntilSuccess(ctx, codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_RECOVERING); err != nil {
		return runContextError(err)
	}
	if err := a.reportInventoryUntilSuccess(ctx); err != nil {
		return runContextError(err)
	}
	if err := a.declareUntilSuccess(ctx, codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE); err != nil {
		return runContextError(err)
	}

	declareTicker := time.NewTicker(a.intervalOrDefault(a.config.DeclareInterval, 5*time.Second))
	defer declareTicker.Stop()
	inventoryTicker := time.NewTicker(defaultInventoryInterval)
	defer inventoryTicker.Stop()
	pollTicker := time.NewTicker(a.intervalOrDefault(a.config.PollInterval, time.Second))
	defer pollTicker.Stop()
	autoStopTicker := time.NewTicker(a.intervalOrDefault(a.config.PollInterval, time.Second))
	defer autoStopTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-a.criticalErrors:
			return err
		case <-declareTicker.C:
			if err := a.declare(ctx, codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE); err != nil {
				if isManagerCriticalError(err) {
					return fmt.Errorf("declare manager: %w", err)
				}
				log.Printf("declare manager: %v", err)
			}
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

func runContextError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (a *Agent) declare(ctx context.Context, state codespacev1.ManagerRuntimeState) error {
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
		CapacityTotal:                      a.config.CapacityTotal,
		CapacityAvailable:                  a.config.CapacityAvailable,
	})
	a.setManagerAuth(request.Header())
	response, err := a.client.DeclareManager(ctx, request)
	if err != nil {
		return fmt.Errorf("declare rpc: %w", err)
	}
	settings, err := validateDeclareResponse(response.Msg)
	if err != nil {
		return err
	}
	if a.settingsStore != nil {
		if err := a.settingsStore.SaveManagerServiceSettings(settings); err != nil {
			return fmt.Errorf("save manager service settings: %w", err)
		}
	}
	return nil
}

func (a *Agent) pollOnce(ctx context.Context) error {
	requestStarted := time.Now()
	requestOperationVersions := a.currentOperationVersions()
	request := connect.NewRequest(&codespacev1.FetchOperationsRequest{
		ProtocolVersion:   protocolVersion,
		CapacityAvailable: a.config.CapacityAvailable,
		AcceptedOperationTypes: []codespacev1.AcceptedOperationType{
			codespacev1.AcceptedOperationType_ACCEPTED_OPERATION_TYPE_CREATE,
			codespacev1.AcceptedOperationType_ACCEPTED_OPERATION_TYPE_RESUME,
		},
		MaxOperations:            a.maxOperations(),
		ObservedOperations:       a.observedOperations(),
		CleanupCapacityAvailable: a.config.CleanupCapacityAvailable,
	})
	a.setManagerAuth(request.Header())
	response, err := a.client.FetchOperations(ctx, request)
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
		duration := leaseDurationFromRequestStart(requestStarted, operation.GetLeaseValidForMilliseconds())
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
	a.updateRuntimeObservations(refs)
	runtimeStates := runtimeStatesByUUID(refs)
	requestOperationVersions := a.currentOperationVersions()
	request := connect.NewRequest(&codespacev1.ReportInstancesRequest{
		ProtocolVersion:     protocolVersion,
		InventoryGeneration: generation,
		Instances:           refs,
	})
	a.setManagerAuth(request.Header())
	response, err := a.client.ReportInstances(ctx, request)
	if err != nil {
		return fmt.Errorf("report instances rpc: %w", err)
	}
	if a.currentInventoryGeneration() != generation {
		return nil
	}
	return a.applyInventoryResults(ctx, generation, runtimeStates, requestOperationVersions, response.Msg.GetResults())
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
	a.autoStopMu.Lock()
	defer a.autoStopMu.Unlock()

	delete(a.autoStops, codespaceUUID)
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
	results []*codespacev1.RuntimeInstanceResult,
) error {
	for _, result := range results {
		if result == nil || result.GetCodespaceUuid() == "" {
			continue
		}
		if a.currentInventoryGeneration() != generation {
			return nil
		}
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
				continue
			}
			if !a.operationVersionAtMost(codespaceUUID, result.GetStopLocalRuntime().GetCurrentOperationRversion()) {
				continue
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
				continue
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
				continue
			}
			log.Printf("inventory requested operation refetch for %s version %d", codespaceUUID, result.GetRefetchOperation().GetCurrentOperationRversion())
		case result.GetReportRuntimeTransition() != nil:
			ok, err := a.validateOperationResponseVersion("inventory action", codespaceUUID, requestOperationVersions, result.GetReportRuntimeTransition().GetCurrentOperationRversion())
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			runtimeState := runtimeStates[codespaceUUID]
			runtimeGeneration, reported, err := a.reportRuntimeTransition(ctx, codespaceUUID, runtimeState, result.GetReportRuntimeTransition().GetCurrentOperationRversion())
			if err != nil {
				return err
			}
			if !reported {
				continue
			}
			if runtimeState == codespacev1.RuntimeState_RUNTIME_STATE_FAILED {
				if err := a.saveCleanupPending(codespaceUUID); err != nil {
					return err
				}
				if err := a.cleanupLocalRuntime(ctx, codespaceUUID); err != nil {
					return err
				}
				continue
			}
			if err := a.clearRuntimeTransitionPending(codespaceUUID, runtimeGeneration); err != nil {
				return fmt.Errorf("clear runtime transition pending %s generation %d: %w", codespaceUUID, runtimeGeneration, err)
			}
		}
	}
	return nil
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
	delete(a.cleanupPendings, codespaceUUID)
	a.runtimeMu.Unlock()
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
	a.runtimeMu.Lock()
	a.cleanupPendings[codespaceUUID] = struct{}{}
	a.runtimeMu.Unlock()
	return nil
}

func (a *Agent) clearDeleteCleanupState(codespaceUUID string) error {
	if a.cleanupStateStore != nil {
		if err := a.cleanupStateStore.ClearCodespaceState(codespaceUUID); err != nil {
			return fmt.Errorf("clear delete cleanup state %s: %w", codespaceUUID, err)
		}
	}
	a.runtimeMu.Lock()
	delete(a.cleanupPendings, codespaceUUID)
	delete(a.runtimeTransitions, codespaceUUID)
	delete(a.runtimeGenerations, codespaceUUID)
	a.runtimeMu.Unlock()
	return nil
}

func (a *Agent) runCleanupPendings(ctx context.Context) error {
	a.runtimeMu.Lock()
	codespaceUUIDs := make([]string, 0, len(a.cleanupPendings))
	for codespaceUUID := range a.cleanupPendings {
		codespaceUUIDs = append(codespaceUUIDs, codespaceUUID)
	}
	a.runtimeMu.Unlock()

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
	request := connect.NewRequest(&codespacev1.ReportRuntimeTransitionRequest{
		ProtocolVersion:           protocolVersion,
		CodespaceUuid:             transition.CodespaceUUID,
		RuntimeGeneration:         transition.RuntimeGeneration,
		ObservedOperationRversion: transition.ObservedOperationRVersion,
		RuntimeState:              transition.TargetState,
	})
	a.setManagerAuth(request.Header())
	if _, err := a.client.ReportRuntimeTransition(ctx, request); err != nil {
		return 0, false, fmt.Errorf("report runtime transition rpc: %w", err)
	}
	return transition.RuntimeGeneration, true, nil
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

func (a *Agent) maxOperations() int32 {
	if a.config.MaxOperations > 0 {
		return a.config.MaxOperations
	}
	return 1
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
		if current.operationRVersion != operationRVersion {
			a.activeMu.Unlock()
			log.Printf("skip operation %s version %d while version %d is active", codespaceUUID, operationRVersion, current.operationRVersion)
			return nil
		}
		if current.running {
			a.activeMu.Unlock()
			return nil
		}
	}
	a.activeMu.Unlock()

	if a.stateStore != nil {
		if err := a.stateStore.SaveActiveOperation(OperationSnapshot{
			Payload:     operation,
			WorkerStage: OperationWorkerStageActive,
		}); err != nil {
			return err
		}
	}

	a.activeMu.Lock()
	if current, ok := a.activeOperations[codespaceUUID]; ok {
		if current.operationRVersion == operationRVersion && !current.running {
			current.payload = operation
			current.running = true
			operationCtx := a.startLeaseLocked(ctx, codespaceUUID, current, leaseDuration)
			a.activeMu.Unlock()
			a.runOperation(operationCtx, operation)
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
		running:           true,
	}
	operationCtx := a.startLeaseLocked(ctx, codespaceUUID, operationContext, leaseDuration)
	a.activeOperations[codespaceUUID] = operationContext
	a.activeMu.Unlock()

	a.runOperation(operationCtx, operation)
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
	operationCtx := a.startLeaseLocked(ctx, lease.GetCodespaceUuid(), current, leaseDuration)
	a.activeMu.Unlock()

	a.runOperation(operationCtx, payload)
	return nil
}

func (a *Agent) runOperation(ctx context.Context, operation *codespacev1.OperationPayload) {
	codespaceUUID := operation.GetCodespaceUuid()
	operationRVersion := operation.GetOperationRversion()
	go func() {
		if err := a.handleOperation(ctx, operation); err != nil {
			critical := isManagerCriticalError(err)
			if ctx.Err() != nil && isStartupOperation(operation) {
				if stopErr := a.provisioner.Stop(context.Background(), runtimeInstanceName(codespaceUUID)); stopErr != nil {
					log.Printf("stop paused operation %s version %d: %v", codespaceUUID, operationRVersion, stopErr)
				}
			}
			a.pauseOperation(codespaceUUID, operationRVersion)
			log.Printf("handle operation %s version %d: %v", codespaceUUID, operationRVersion, err)
			if critical {
				a.reportCriticalError(fmt.Errorf("operation %s version %d: %w", codespaceUUID, operationRVersion, err))
			}
			return
		}
		a.finishOperation(codespaceUUID, operationRVersion)
	}()
}

func (a *Agent) reportCriticalError(err error) {
	select {
	case a.criticalErrors <- err:
	default:
	}
}

func (a *Agent) finishOperation(codespaceUUID string, operationRVersion int64) {
	if a.stateStore != nil {
		if err := a.stateStore.DeleteActiveOperation(codespaceUUID, operationRVersion); err != nil {
			log.Printf("delete operation state %s version %d: %v", codespaceUUID, operationRVersion, err)
		}
	}
	a.activeMu.Lock()
	defer a.activeMu.Unlock()

	if current, ok := a.activeOperations[codespaceUUID]; ok && current.operationRVersion == operationRVersion {
		a.stopLeaseLocked(current)
		delete(a.activeOperations, codespaceUUID)
	}
}

func (a *Agent) pauseOperation(codespaceUUID string, operationRVersion int64) {
	a.activeMu.Lock()
	current, ok := a.activeOperations[codespaceUUID]
	if ok && current.operationRVersion == operationRVersion {
		a.stopLeaseLocked(current)
		current.running = false
	}
	var payload *codespacev1.OperationPayload
	if ok && current.operationRVersion == operationRVersion {
		payload = current.payload
	}
	a.activeMu.Unlock()

	if payload != nil && a.stateStore != nil {
		if err := a.stateStore.SaveActiveOperation(OperationSnapshot{
			Payload:     payload,
			WorkerStage: OperationWorkerStageLeasePaused,
		}); err != nil {
			log.Printf("pause operation state %s version %d: %v", codespaceUUID, operationRVersion, err)
		}
	}
}

func (a *Agent) startLeaseLocked(ctx context.Context, codespaceUUID string, operation *operationContext, leaseDuration time.Duration) context.Context {
	a.stopLeaseLocked(operation)
	operationCtx, cancel := context.WithCancel(ctx)
	operation.cancel = cancel
	operation.leaseTimer = time.AfterFunc(leaseDuration, func() {
		log.Printf("operation %s version %d local lease expired", codespaceUUID, operation.operationRVersion)
		cancel()
	})
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

func (a *Agent) handleOperation(ctx context.Context, operation *codespacev1.OperationPayload) error {
	if err := a.updateLog(ctx, operation, "operation started"); err != nil {
		return err
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
		err = a.handleDelete(ctx, operation, true)
	case *codespacev1.OperationPayload_AbortCreate:
		err = a.handleDelete(ctx, operation, false)
	case *codespacev1.OperationPayload_AbortResume:
		err = a.handleStop(ctx, operation)
	default:
		err = fmt.Errorf("operation command is missing")
	}

	finalStatus := codespacev1.FinalStatus_FINAL_STATUS_DONE
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isManagerCriticalError(err) {
			return err
		}
		if logErr := a.updateLog(ctx, operation, err.Error()); isManagerCriticalError(logErr) {
			return logErr
		}
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

func (a *Agent) handleResourceAbsentFinal(ctx context.Context, operation *codespacev1.OperationPayload) {
	if ctx.Err() != nil {
		return
	}
	a.finishOperation(operation.GetCodespaceUuid(), operation.GetOperationRversion())
	a.triggerResourceAbsentInventory(context.Background(), operation)
}

func (a *Agent) handleCreate(ctx context.Context, operation *codespacev1.OperationPayload, payload *codespacev1.CreateOperationPayload) error {
	a.applyRuntimeSettings(operation.GetCodespaceUuid(), payload.GetRuntimeSettings(), time.Now())
	instance, err := a.provisioner.CreateOrStart(ctx, provisioner.InstanceSpec{
		CodespaceUUID: operation.GetCodespaceUuid(),
		Name:          runtimeInstanceName(operation.GetCodespaceUuid()),
		RepoFullName:  payload.GetRepoFullName(),
		RepoTag:       payload.GetRepoTag(),
	})
	if err != nil {
		return err
	}
	token, err := a.requestGiteaToken(ctx, operation.GetCodespaceUuid())
	if err != nil {
		return err
	}
	if err := a.provisioner.Bootstrap(ctx, instance.Name, provisioner.BootstrapRequest{
		CodespaceUUID:    operation.GetCodespaceUuid(),
		GiteaToken:       token.GetToken(),
		ServerURL:        token.GetServerUrl(),
		RepoCloneHTTPURL: payload.GetRepoCloneHttpUrl(),
		RepoCloneSSHURL:  payload.GetRepoCloneSshUrl(),
		RepoFullName:     payload.GetRepoFullName(),
		StartRef:         payload.GetStartRef(),
		CommitSHA:        payload.GetCommitSha(),
		Workdir:          instance.Workdir,
		GitProtocol:      payload.GetGitProtocol().String(),
	}); err != nil {
		return err
	}
	if err := a.reportReadyMetadata(ctx, operation, instance); err != nil {
		return err
	}
	a.markRuntimeReady(operation.GetCodespaceUuid())
	return nil
}

func (a *Agent) handleResume(ctx context.Context, operation *codespacev1.OperationPayload, payload *codespacev1.ResumeOperationPayload) error {
	a.applyRuntimeSettings(operation.GetCodespaceUuid(), payload.GetRuntimeSettings(), time.Now())
	instance, err := a.provisioner.StartExisting(ctx, provisioner.InstanceSpec{
		CodespaceUUID: operation.GetCodespaceUuid(),
		Name:          runtimeInstanceName(operation.GetCodespaceUuid()),
	})
	if err != nil {
		return err
	}
	if _, err := a.requestGiteaToken(ctx, operation.GetCodespaceUuid()); err != nil {
		return err
	}
	if err := a.reportReadyMetadata(ctx, operation, instance); err != nil {
		return err
	}
	a.markRuntimeReady(operation.GetCodespaceUuid())
	return nil
}

func (a *Agent) handleStop(ctx context.Context, operation *codespacev1.OperationPayload) error {
	if err := a.provisioner.Stop(ctx, runtimeInstanceName(operation.GetCodespaceUuid())); err != nil {
		return err
	}
	a.markRuntimeStopped(operation.GetCodespaceUuid())
	return nil
}

func (a *Agent) handleDelete(ctx context.Context, operation *codespacev1.OperationPayload, cleanupPending bool) error {
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

func (a *Agent) requestGiteaToken(ctx context.Context, codespaceUUID string) (*codespacev1.RequestGiteaTokenResponse, error) {
	request := connect.NewRequest(&codespacev1.RequestGiteaTokenRequest{
		ProtocolVersion: protocolVersion,
		CodespaceUuid:   codespaceUUID,
	})
	a.setManagerAuth(request.Header())
	response, err := a.client.RequestGiteaToken(ctx, request)
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
	response, err := a.client.RequestIdleStop(ctx, request)
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

func (a *Agent) reportReadyMetadata(ctx context.Context, operation *codespacev1.OperationPayload, instance *provisioner.Instance) error {
	if instance == nil {
		return fmt.Errorf("runtime instance is required")
	}
	now := time.Now().Unix()
	metadata := map[string]any{
		"runtime": map[string]any{
			"internal_ssh": map[string]any{
				"host":                 instance.InternalSSHHost,
				"port":                 instance.InternalSSHPort,
				"user":                 instance.InternalSSHUser,
				"auth_mode":            instance.InternalSSHAuthMode,
				"host_key_fingerprint": instance.InternalSSHFingerprint,
			},
		},
		"endpoints": []any{},
		"boot": map[string]any{
			"stage":              "ready",
			"operation_rversion": operation.GetOperationRversion(),
			"started_unix":       now,
			"last_update_unix":   now,
		},
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode runtime metadata: %w", err)
	}
	request := connect.NewRequest(&codespacev1.ReportRuntimeMetadataRequest{
		ProtocolVersion:    protocolVersion,
		CodespaceUuid:      operation.GetCodespaceUuid(),
		MetadataJson:       string(encoded),
		MetadataGeneration: a.nextRuntimeMetadataGeneration(),
	})
	a.setManagerAuth(request.Header())
	if _, err := a.client.ReportRuntimeMetadata(ctx, request); err != nil {
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
	request := connect.NewRequest(&codespacev1.UpdateLogRequest{
		ProtocolVersion:   protocolVersion,
		CodespaceUuid:     operation.GetCodespaceUuid(),
		OperationRversion: operation.GetOperationRversion(),
		Offset:            operation.GetLogOffset(),
		Lines: []*codespacev1.LogLine{{
			TimestampUnixNano: time.Now().UnixNano(),
			Message:           message,
		}},
	})
	a.setManagerAuth(request.Header())
	if _, err := a.client.UpdateLog(ctx, request); err != nil {
		return fmt.Errorf("update log rpc: %w", err)
	}
	return nil
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
	response, err := a.client.FinalizeOperation(ctx, request)
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
