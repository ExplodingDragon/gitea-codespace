// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/controlplane"
	"gitea.dev/codespace/internal/provisioner"
)

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
		ProtocolVersion:     controlplane.ProtocolVersion,
		InventoryGeneration: generation,
		Instances:           refs,
	})
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
			RuntimeUuid:               instance.CodespaceUUID,
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
		if ref == nil || ref.GetRuntimeUuid() == "" {
			continue
		}
		states[ref.GetRuntimeUuid()] = ref.GetRuntimeState()
	}
	return states
}

func runtimeHealthCandidates(refs []*codespacev1.RuntimeInstanceRef) map[string]struct{} {
	candidates := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref == nil ||
			ref.GetRuntimeUuid() == "" ||
			ref.GetRuntimeState() != codespacev1.RuntimeState_RUNTIME_STATE_RUNNING {
			continue
		}
		candidates[ref.GetRuntimeUuid()] = struct{}{}
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
		if ref == nil || ref.GetRuntimeUuid() == "" {
			continue
		}
		codespaceUUID := ref.GetRuntimeUuid()
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

func (a *Agent) applyInventoryResults(
	ctx context.Context,
	generation int64,
	runtimeStates map[string]codespacev1.RuntimeState,
	requestOperationVersions map[string]int64,
	healthCandidates map[string]struct{},
	results []*codespacev1.RuntimeInstanceResult,
) error {
	for _, result := range results {
		if result == nil || result.GetRuntimeUuid() == "" {
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
	codespaceUUID := result.GetRuntimeUuid()
	if result.GetRuntimeSettings() != nil {
		a.applyRuntimeSettings(codespaceUUID, result.GetRuntimeSettings(), time.Now())
	}
	currentOperationRVersion := result.GetCurrentOperationRversion()
	switch result.GetAction() {
	case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_CLEANUP_LOCAL_RUNTIME:
		if err := a.saveCleanupPending(codespaceUUID); err != nil {
			return err
		}
		if err := a.clearOperationContext(codespaceUUID, 0); err != nil {
			return err
		}
		if err := a.cleanupLocalRuntime(ctx, codespaceUUID); err != nil {
			return err
		}
	case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_STOP_LOCAL_RUNTIME:
		ok, err := a.validateOperationResponseVersion("inventory action", codespaceUUID, requestOperationVersions, currentOperationRVersion)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if !a.operationVersionAtMost(codespaceUUID, currentOperationRVersion) {
			return nil
		}
		if err := a.deactivateRuntimeMetadata(ctx, codespaceUUID); err != nil {
			return err
		}
		if err := a.provisioner.Stop(ctx, runtimeInstanceName(codespaceUUID)); err != nil {
			return fmt.Errorf("stop local runtime %s: %w", codespaceUUID, err)
		}
	case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_CLEAR_OPERATION_CONTEXT:
		ok, err := a.validateOperationResponseVersion("inventory action", codespaceUUID, requestOperationVersions, currentOperationRVersion)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := a.clearOperationContext(codespaceUUID, currentOperationRVersion); err != nil {
			return err
		}
	case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_REFETCH_OPERATION:
		ok, err := a.validateOperationResponseVersion("inventory action", codespaceUUID, requestOperationVersions, currentOperationRVersion)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		log.Printf("inventory requested operation refetch for %s version %d", codespaceUUID, currentOperationRVersion)
	case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_REPORT_RUNTIME_TRANSITION:
		ok, err := a.validateOperationResponseVersion("inventory action", codespaceUUID, requestOperationVersions, currentOperationRVersion)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		runtimeState := runtimeStates[codespaceUUID]
		if err := a.deactivateRuntimeMetadata(ctx, codespaceUUID); err != nil {
			return err
		}
		runtimeGeneration, reported, err := a.reportRuntimeTransition(ctx, codespaceUUID, runtimeState, currentOperationRVersion)
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
	case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_UNSPECIFIED:
		if err := a.repairStableRunningRuntime(ctx, codespaceUUID, runtimeStates, requestOperationVersions, healthCandidates); err != nil {
			return err
		}
		if runtimeStates[codespaceUUID] == codespacev1.RuntimeState_RUNTIME_STATE_RUNNING && requestOperationVersions[codespaceUUID] == 0 && a.metadataPublisher != nil {
			active, err := a.metadataPublisher.ActivateRuntimeMetadata(codespaceUUID)
			if err != nil {
				return fmt.Errorf("activate stable runtime metadata %s: %w", codespaceUUID, err)
			}
			if active {
				a.markRuntimeReady(codespaceUUID)
			}
		}
	default:
		return fmt.Errorf("inventory action for %s is invalid", codespaceUUID)
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
		observedOperationRVersion, observedErr := a.stableRunningObservedOperationVersion(codespaceUUID)
		if observedErr != nil {
			return observedErr
		}
		if err := a.deactivateRuntimeMetadata(ctx, codespaceUUID); err != nil {
			return err
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
		observedOperationRVersion, observedErr := a.stableRunningObservedOperationVersion(codespaceUUID)
		if observedErr != nil {
			return observedErr
		}
		if err := a.deactivateRuntimeMetadata(ctx, codespaceUUID); err != nil {
			return err
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
	if !ok || a.runtimeEnvStateStore == nil {
		return nil
	}
	environment, ok, err := a.runtimeEnvStateStore.LoadRuntimeEnvironment(codespaceUUID)
	if err != nil {
		return fmt.Errorf("load runtime environment %s: %w", codespaceUUID, err)
	}
	if !ok {
		return fmt.Errorf("runtime environment is missing")
	}
	workdir := strings.TrimSpace(environment.Environment.Workspace)
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
	if a.runtimeHealthStore == nil || codespaceUUID == "" {
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
	var healthErr error
	if checker, ok := a.provisioner.(workspaceAccessChecker); ok {
		healthErr = a.checkRuntimeWorkspaceAccess(ctx, checker, snapshot)
	}
	if healthErr == nil {
		healthErr = a.checkRuntimeDevelopmentEnvironment(ctx, codespaceUUID, snapshot.InstanceName)
	}
	if healthErr == nil {
		a.clearRuntimeHealthFailure(codespaceUUID)
		return nil
	}
	failures := a.recordRuntimeHealthFailure(codespaceUUID)
	if failures < runtimeHealthFailuresBeforeStop {
		a.closeCodespaceAccess(codespaceUUID)
		log.Printf("runtime health check failed for %s (%d/%d): %v", codespaceUUID, failures, runtimeHealthFailuresBeforeStop, healthErr)
		return nil
	}
	log.Printf("runtime health check failed for %s (%d/%d), stopping runtime: %v", codespaceUUID, failures, runtimeHealthFailuresBeforeStop, healthErr)

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
	if err := a.deactivateRuntimeMetadata(ctx, pending.CodespaceUUID); err != nil {
		return 0, false, err
	}
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
	if err := a.deactivateRuntimeMetadata(ctx, codespaceUUID); err != nil {
		return err
	}
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
	if a.runtimeIdentityStore != nil {
		if err := a.runtimeIdentityStore.DeleteRuntimeIdentity(ctx, codespaceUUID); err != nil {
			return &categorizedError{category: failureLocalStateCommit, message: fmt.Sprintf("delete runtime identity %s: %v", codespaceUUID, err)}
		}
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

func (a *Agent) clearDeleteCleanupState(ctx context.Context, codespaceUUID string) error {
	if a.runtimeIdentityStore != nil {
		if err := a.runtimeIdentityStore.DeleteRuntimeIdentity(ctx, codespaceUUID); err != nil {
			return &categorizedError{category: failureLocalStateCommit, message: fmt.Sprintf("delete runtime identity %s: %v", codespaceUUID, err)}
		}
	}
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
		ProtocolVersion:           controlplane.ProtocolVersion,
		RuntimeUuid:               transition.CodespaceUUID,
		RuntimeGeneration:         transition.RuntimeGeneration,
		ObservedOperationRversion: transition.ObservedOperationRVersion,
		RuntimeState:              transition.TargetState,
	})
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
