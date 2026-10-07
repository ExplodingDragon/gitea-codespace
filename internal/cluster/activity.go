// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"fmt"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

const (
	gatewayActivityTTL   = 30 * time.Second
	idleStopRetryDelay   = 5 * time.Second
	idleStopPendingDelay = 30 * time.Second
)

type gatewayActivityKey struct {
	componentUID string
	instance     string
}

type gatewayActivityReport struct {
	counts   map[string]int64
	reported time.Time
}

type runtimeActivity struct {
	siteUID               string
	autoStopEnabled       bool
	idleTimeoutSeconds    int64
	interactionGeneration int64
	hasPolicy             bool
	activityAfter         time.Time
	idleSince             time.Time
	retryAt               time.Time
}

// RuntimeActivityTracker joins ephemeral Gateway sessions with Gitea's
// authoritative auto-stop policy for the current Manager leader term.
type RuntimeActivityTracker struct {
	mu       sync.Mutex
	gateways map[gatewayActivityKey]gatewayActivityReport
	runtimes map[string]*runtimeActivity
}

func (t *RuntimeActivityTracker) ObserveSettings(siteUID, runtimeUUID string, settings *codespacev1.EffectiveCodespaceRuntimeSettings, now time.Time) error {
	if siteUID == "" || runtimeUUID == "" || validateRuntimeSettings(settings) != nil {
		return fmt.Errorf("invalid effective Runtime settings")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.runtimeLocked(siteUID, runtimeUUID)
	if settings.InteractionGeneration < state.interactionGeneration {
		return nil
	}
	changed := !state.hasPolicy || settings.AutoStopEnabled != state.autoStopEnabled || settings.IdleTimeoutSeconds != state.idleTimeoutSeconds || settings.InteractionGeneration != state.interactionGeneration
	state.autoStopEnabled = settings.AutoStopEnabled
	state.idleTimeoutSeconds = settings.IdleTimeoutSeconds
	state.interactionGeneration = settings.InteractionGeneration
	state.hasPolicy = true
	if changed || !settings.AutoStopEnabled {
		state.activityAfter = now
		state.idleSince = time.Time{}
		state.retryAt = time.Time{}
	}
	return nil
}

func (t *RuntimeActivityTracker) ObserveInteraction(siteUID, runtimeUUID string, generation int64, now time.Time) {
	if siteUID == "" || runtimeUUID == "" || generation < 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.runtimeLocked(siteUID, runtimeUUID)
	if generation <= state.interactionGeneration {
		return
	}
	state.interactionGeneration = generation
	state.activityAfter = now
	state.idleSince = time.Time{}
	state.retryAt = time.Time{}
}

func (t *RuntimeActivityTracker) ReplaceGateway(componentUID, instance string, counts map[string]int64, release bool, now time.Time) error {
	if componentUID == "" || instance == "" {
		return fmt.Errorf("gateway activity identity is incomplete")
	}
	key := gatewayActivityKey{componentUID: componentUID, instance: instance}
	t.mu.Lock()
	defer t.mu.Unlock()
	if release {
		delete(t.gateways, key)
		return nil
	}
	copyCounts := make(map[string]int64, len(counts))
	for runtimeUUID, count := range counts {
		if runtimeUUID == "" || count < 0 {
			return fmt.Errorf("gateway activity count is invalid")
		}
		if count > 0 {
			copyCounts[runtimeUUID] = count
		}
	}
	if t.gateways == nil {
		t.gateways = make(map[gatewayActivityKey]gatewayActivityReport)
	}
	t.gateways[key] = gatewayActivityReport{counts: copyCounts, reported: now}
	return nil
}

func (t *RuntimeActivityTracker) IdleRequest(siteUID, runtimeUUID, gatewayUID string, eligible bool, now time.Time) (*codespacev1.EffectiveCodespaceRuntimeSettings, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.runtimes[runtimeUUID]
	if state == nil || state.siteUID != siteUID || !state.hasPolicy || !state.autoStopEnabled || !eligible {
		if state != nil {
			state.idleSince = time.Time{}
			state.retryAt = time.Time{}
		}
		return nil, false
	}
	fresh, sessions := false, int64(0)
	for key, report := range t.gateways {
		if now.Sub(report.reported) > gatewayActivityTTL {
			delete(t.gateways, key)
			continue
		}
		if key.componentUID == gatewayUID && !report.reported.Before(state.activityAfter) {
			fresh = true
			sessions += report.counts[runtimeUUID]
		}
	}
	if !fresh || sessions > 0 {
		state.idleSince = time.Time{}
		state.retryAt = time.Time{}
		return nil, false
	}
	if state.idleSince.IsZero() {
		state.idleSince = now
		return nil, false
	}
	if now.Sub(state.idleSince) < time.Duration(state.idleTimeoutSeconds)*time.Second || now.Before(state.retryAt) {
		return nil, false
	}
	state.retryAt = now.Add(idleStopRetryDelay)
	return &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       state.autoStopEnabled,
		IdleTimeoutSeconds:    state.idleTimeoutSeconds,
		InteractionGeneration: state.interactionGeneration,
	}, true
}

func (t *RuntimeActivityTracker) IdleStopResult(siteUID, runtimeUUID string, response *codespacev1.RequestIdleStopResponse, now time.Time) error {
	if response == nil {
		return fmt.Errorf("idle stop response is missing")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.runtimes[runtimeUUID]
	if state == nil || state.siteUID != siteUID {
		return nil
	}
	switch outcome := response.Outcome.(type) {
	case *codespacev1.RequestIdleStopResponse_Pending:
		if outcome.Pending == nil || outcome.Pending.OperationRversion <= 0 {
			return fmt.Errorf("idle stop pending result is invalid")
		}
		state.retryAt = now.Add(idleStopPendingDelay)
	case *codespacev1.RequestIdleStopResponse_ObservationChanged:
		settings := outcome.ObservationChanged.GetRuntimeSettings()
		if validateRuntimeSettings(settings) != nil {
			return fmt.Errorf("idle stop observation is invalid")
		}
		if !state.hasPolicy || settings.InteractionGeneration >= state.interactionGeneration {
			changed := !state.hasPolicy || settings.AutoStopEnabled != state.autoStopEnabled || settings.IdleTimeoutSeconds != state.idleTimeoutSeconds || settings.InteractionGeneration != state.interactionGeneration
			state.autoStopEnabled = settings.AutoStopEnabled
			state.idleTimeoutSeconds = settings.IdleTimeoutSeconds
			state.interactionGeneration = settings.InteractionGeneration
			state.hasPolicy = true
			if changed || !settings.AutoStopEnabled {
				state.activityAfter = now
				state.idleSince = time.Time{}
			}
		}
		state.retryAt = time.Time{}
	case *codespacev1.RequestIdleStopResponse_NotApplicable:
		if outcome.NotApplicable == nil || outcome.NotApplicable.Reason == codespacev1.IdleStopNotApplicableReason_IDLE_STOP_NOT_APPLICABLE_REASON_UNSPECIFIED {
			return fmt.Errorf("idle stop not-applicable result is invalid")
		}
		state.idleSince = time.Time{}
		state.retryAt = now.Add(idleStopRetryDelay)
	default:
		return fmt.Errorf("idle stop response outcome is missing")
	}
	return nil
}

func validateRuntimeSettings(settings *codespacev1.EffectiveCodespaceRuntimeSettings) error {
	if settings == nil || settings.InteractionGeneration < 0 || settings.AutoStopEnabled && settings.IdleTimeoutSeconds <= 0 || !settings.AutoStopEnabled && settings.IdleTimeoutSeconds != 0 {
		return fmt.Errorf("invalid effective Runtime settings")
	}
	return nil
}

func (t *RuntimeActivityTracker) IdleStopFailed(runtimeUUID string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if state := t.runtimes[runtimeUUID]; state != nil {
		state.retryAt = now.Add(idleStopRetryDelay)
	}
}

func (t *RuntimeActivityTracker) RetainSite(siteUID string, current map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for runtimeUUID, state := range t.runtimes {
		if state.siteUID == siteUID && !current[runtimeUUID] {
			delete(t.runtimes, runtimeUUID)
		}
	}
}

func (t *RuntimeActivityTracker) runtimeLocked(siteUID, runtimeUUID string) *runtimeActivity {
	if t.runtimes == nil {
		t.runtimes = make(map[string]*runtimeActivity)
	}
	state := t.runtimes[runtimeUUID]
	if state == nil || state.siteUID != siteUID {
		state = &runtimeActivity{siteUID: siteUID}
		t.runtimes[runtimeUUID] = state
	}
	return state
}
