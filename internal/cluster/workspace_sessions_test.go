// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceSessionActivityAcrossGatewayReplicas(t *testing.T) {
	start := time.Unix(100, 0)
	tracker := &workspaceSessionTracker{}
	identity := &componentv1.WorkspaceSessionIdentity{SessionId: "browser", SiteUid: "site", UserId: 42, RuntimeUuid: "runtime"}
	entry := &componentv1.WorkspaceSessionActivity{Identity: identity, Active: true, LastActiveUnixMilliseconds: start.UnixMilli()}
	first := &workspaceSessionReport{idleTimeout: time.Minute}
	second := &workspaceSessionReport{idleTimeout: time.Minute}
	a := gatewayActivityKey{componentUID: "gateway", instance: "a"}
	b := gatewayActivityKey{componentUID: "gateway", instance: "b"}
	require.NoError(t, tracker.replace(a, first, []*componentv1.WorkspaceSessionActivity{entry}, start))
	tracker.renew(a, first, start, time.Minute)
	require.NoError(t, tracker.replace(b, second, []*componentv1.WorkspaceSessionActivity{entry}, start))
	tracker.renew(b, second, start, time.Minute)

	now := start.Add(9 * time.Hour)
	require.NoError(t, tracker.replace(b, second, []*componentv1.WorkspaceSessionActivity{entry}, now))
	tracker.renew(b, second, now, time.Minute)
	renewable, err := tracker.lookup("gateway", identity, now)
	require.NoError(t, err)
	require.True(t, renewable)
	renewable, err = tracker.lookup("other-gateway", identity, now)
	require.NoError(t, err)
	require.False(t, renewable)
	other := &componentv1.WorkspaceSessionIdentity{SessionId: "browser", SiteUid: "site", UserId: 43, RuntimeUuid: "runtime"}
	renewable, err = tracker.lookup("gateway", other, now)
	require.NoError(t, err)
	require.False(t, renewable)

	tracker.disconnect(b, second)
	_, err = tracker.lookup("gateway", identity, now.Add(time.Second))
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
	replacement := &workspaceSessionReport{idleTimeout: time.Minute}
	require.NoError(t, tracker.replace(b, replacement, []*componentv1.WorkspaceSessionActivity{entry}, now.Add(2*time.Second)))
	tracker.renew(b, replacement, now.Add(2*time.Second), time.Minute)
	tracker.disconnect(b, second)
	require.True(t, replacement.connected)
	require.Equal(t, connect.CodeCanceled, connect.CodeOf(tracker.replace(b, second, nil, now.Add(3*time.Second))))

	entry.Active = false
	entry.LastActiveUnixMilliseconds = now.Add(4 * time.Second).UnixMilli()
	require.NoError(t, tracker.replace(b, replacement, []*componentv1.WorkspaceSessionActivity{entry}, now.Add(4*time.Second)))
	// Repeating a snapshot must not move the recorded last activity.
	require.NoError(t, tracker.replace(b, replacement, []*componentv1.WorkspaceSessionActivity{entry}, now.Add(50*time.Second)))
	renewable, err = tracker.lookup("gateway", identity, now.Add(63*time.Second))
	require.NoError(t, err)
	require.True(t, renewable)
	renewable, err = tracker.lookup("gateway", identity, now.Add(64*time.Second))
	require.NoError(t, err)
	require.False(t, renewable)
}

func TestWorkspaceSessionActivityRecoveryAndPermitExpiry(t *testing.T) {
	start := time.Unix(100, 0)
	tracker := &workspaceSessionTracker{}
	key := gatewayActivityKey{componentUID: "gateway", instance: "a"}
	identity := &componentv1.WorkspaceSessionIdentity{SessionId: "browser", SiteUid: "site", UserId: 42, RuntimeUuid: "runtime"}
	owner := &workspaceSessionReport{idleTimeout: time.Minute}
	require.NoError(t, tracker.replace(key, owner, nil, start))
	_, err := tracker.lookup("gateway", identity, start.Add(time.Second))
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
	renewable, err := tracker.lookup("gateway", identity, start.Add(gatewayPermitTTL))
	require.NoError(t, err)
	require.False(t, renewable)

	entry := &componentv1.WorkspaceSessionActivity{Identity: identity, Active: true, LastActiveUnixMilliseconds: start.UnixMilli()}
	require.NoError(t, tracker.replace(key, owner, []*componentv1.WorkspaceSessionActivity{entry}, start.Add(time.Minute)))
	tracker.renew(key, owner, start.Add(time.Minute), time.Minute)
	tracker.disconnect(key, owner)
	renewable, err = tracker.lookup("gateway", identity, start.Add(100*time.Second))
	require.NoError(t, err)
	require.True(t, renewable, "last known activity permits reconnection after losing a replica")
	renewable, err = tracker.lookup("gateway", identity, start.Add(2*time.Minute))
	require.NoError(t, err)
	require.False(t, renewable)
	require.Empty(t, tracker.reports)
}
