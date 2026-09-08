// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"context"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

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
