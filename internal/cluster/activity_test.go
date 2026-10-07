// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"github.com/stretchr/testify/require"
)

func TestRuntimeActivityTrackerRequiresFreshIdleGateway(t *testing.T) {
	tracker := &RuntimeActivityTracker{}
	start := time.Now()
	settings := &codespacev1.EffectiveCodespaceRuntimeSettings{AutoStopEnabled: true, IdleTimeoutSeconds: 10, InteractionGeneration: 1}
	require.NoError(t, tracker.ObserveSettings("site", "runtime", settings, start))

	_, ready := tracker.IdleRequest("site", "runtime", "gateway", true, start)
	require.False(t, ready)
	require.NoError(t, tracker.ReplaceGateway("gateway", "pod-1", nil, false, start))
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, start)
	require.False(t, ready)
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, start.Add(9*time.Second))
	require.False(t, ready)
	observed, ready := tracker.IdleRequest("site", "runtime", "gateway", true, start.Add(10*time.Second))
	require.True(t, ready)
	require.Equal(t, settings, observed)

	tracker.ObserveInteraction("site", "runtime", 2, start.Add(11*time.Second))
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, start.Add(12*time.Second))
	require.False(t, ready)
	require.NoError(t, tracker.ReplaceGateway("gateway", "pod-1", nil, false, start.Add(12*time.Second)))
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, start.Add(20*time.Second))
	require.False(t, ready)
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, start.Add(30*time.Second))
	require.True(t, ready)

	require.NoError(t, tracker.ReplaceGateway("gateway", "pod-1", nil, false, start))
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, start.Add(gatewayActivityTTL+time.Second))
	require.False(t, ready)
}

func TestRuntimeActivityTrackerAppliesIdleStopOutcomes(t *testing.T) {
	tracker := &RuntimeActivityTracker{}
	now := time.Now()
	require.NoError(t, tracker.ObserveSettings("site", "runtime", &codespacev1.EffectiveCodespaceRuntimeSettings{AutoStopEnabled: true, IdleTimeoutSeconds: 1, InteractionGeneration: 1}, now))
	require.NoError(t, tracker.ReplaceGateway("gateway", "pod", nil, false, now))
	_, ready := tracker.IdleRequest("site", "runtime", "gateway", true, now)
	require.False(t, ready)
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, now.Add(time.Second))
	require.True(t, ready)

	response := &codespacev1.RequestIdleStopResponse{Outcome: &codespacev1.RequestIdleStopResponse_ObservationChanged{ObservationChanged: &codespacev1.IdleStopObservationChanged{RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{AutoStopEnabled: true, IdleTimeoutSeconds: 5, InteractionGeneration: 2}}}}
	require.NoError(t, tracker.IdleStopResult("site", "runtime", response, now.Add(time.Second)))
	require.NoError(t, tracker.ReplaceGateway("gateway", "pod", nil, false, now.Add(2*time.Second)))
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, now.Add(6*time.Second))
	require.False(t, ready)
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, now.Add(11*time.Second))
	require.True(t, ready)

	response = &codespacev1.RequestIdleStopResponse{Outcome: &codespacev1.RequestIdleStopResponse_Pending{Pending: &codespacev1.IdleStopPending{OperationRversion: 4}}}
	require.NoError(t, tracker.IdleStopResult("site", "runtime", response, now.Add(11*time.Second)))
	_, ready = tracker.IdleRequest("site", "runtime", "gateway", true, now.Add(12*time.Second))
	require.False(t, ready)
}
