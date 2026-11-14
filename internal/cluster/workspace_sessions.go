// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Workspace activity is ephemeral and separate from Runtime auto-stop counts.
// Each control stream owns its report, so a replaced stream cannot erase its successor.
type workspaceSessionTracker struct {
	mu      sync.Mutex
	started time.Time
	reports map[gatewayActivityKey]*workspaceSessionReport
}

type workspaceSessionReport struct {
	sessions    map[string]*componentv1.WorkspaceSessionActivity
	idleTimeout time.Duration
	reported    time.Time
	permitUntil time.Time
	connected   bool
}

func (s *ComponentServer) replaceWorkspaceActivity(key gatewayActivityKey, owner *workspaceSessionReport, entries []*componentv1.WorkspaceSessionActivity, authorizedSites map[string]string) error {
	filtered := make([]*componentv1.WorkspaceSessionActivity, 0, len(entries))
	for _, entry := range entries {
		identity := entry.GetIdentity()
		if site, ok := authorizedSites[identity.GetRuntimeUuid()]; ok && site == identity.GetSiteUid() {
			filtered = append(filtered, entry)
		}
	}
	return s.workspaceSessions.replace(key, owner, filtered, time.Now())
}

func (t *workspaceSessionTracker) replace(key gatewayActivityKey, owner *workspaceSessionReport, entries []*componentv1.WorkspaceSessionActivity, now time.Time) error {
	sessions := make(map[string]*componentv1.WorkspaceSessionActivity, len(entries))
	for _, entry := range entries {
		identity := entry.GetIdentity()
		if identity.GetSessionId() == "" || identity.GetSiteUid() == "" || identity.GetUserId() <= 0 || identity.GetRuntimeUuid() == "" || entry.LastActiveUnixMilliseconds <= 0 {
			return connect.NewError(connect.CodeInvalidArgument, "workspace session activity is incomplete")
		}
		if _, duplicate := sessions[identity.SessionId]; duplicate {
			return connect.NewError(connect.CodeInvalidArgument, "workspace session activity is duplicated")
		}
		sessions[identity.SessionId] = proto.Clone(entry).(*componentv1.WorkspaceSessionActivity)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started.IsZero() {
		t.started = now
	}
	if t.reports == nil {
		t.reports = make(map[gatewayActivityKey]*workspaceSessionReport)
	}
	if current := t.reports[key]; current != owner && !owner.reported.IsZero() {
		return connect.NewError(connect.CodeCanceled, "gateway control stream was replaced")
	}
	owner.sessions, owner.reported, owner.connected = sessions, now, true
	t.reports[key] = owner
	t.pruneLocked(now)
	return nil
}

func (t *workspaceSessionTracker) renew(key gatewayActivityKey, owner *workspaceSessionReport, now time.Time, idleTimeout time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reports[key] == owner {
		owner.permitUntil = now.Add(gatewayPermitTTL)
		owner.idleTimeout = idleTimeout
	}
}

func (t *workspaceSessionTracker) disconnect(key gatewayActivityKey, owner *workspaceSessionReport) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reports[key] != owner {
		return false
	}
	owner.connected = false
	return true
}

func (t *workspaceSessionTracker) lookup(componentUID string, identity *componentv1.WorkspaceSessionIdentity, now time.Time) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	uncertain := t.started.IsZero() || now.Sub(t.started) < gatewayPermitTTL
	for key, report := range t.reports {
		if key.componentUID != componentUID {
			continue
		}
		entry := report.sessions[identity.SessionId]
		if entry == nil || !proto.Equal(entry.Identity, identity) {
			continue
		}
		if entry.Active && now.Before(report.permitUntil) && (!report.connected || now.Sub(report.reported) >= gatewayActivityTTL) {
			uncertain = true
			continue
		}
		// Pruning already checked the idle window; a live owner must also have a fresh report.
		return true, nil
	}
	if uncertain {
		return false, connect.NewError(connect.CodeUnavailable, "workspace session activity is synchronizing")
	}
	return false, nil
}

func (t *workspaceSessionTracker) pruneLocked(now time.Time) {
	for key, report := range t.reports {
		for id, entry := range report.sessions {
			lastActive := time.UnixMilli(entry.LastActiveUnixMilliseconds)
			if entry.Active {
				if now.Before(report.permitUntil) {
					continue
				}
				// After losing the owner, its last report is the last proven activity.
				lastActive = report.reported
			}
			if now.Sub(lastActive) >= report.idleTimeout {
				delete(report.sessions, id)
			}
		}
		if !report.connected && len(report.sessions) == 0 && !now.Before(report.permitUntil) {
			delete(t.reports, key)
		}
	}
}

func (s *ComponentServer) LookupWorkspaceSession(ctx context.Context, request *componentv1.LookupWorkspaceSessionRequest) (*componentv1.LookupWorkspaceSessionResponse, error) {
	component, err := componentIdentityFromContext(ctx, "gateway")
	if err != nil {
		return nil, err
	}
	identity := request.GetIdentity()
	if request.ProtocolVersion != 1 || identity.GetSessionId() == "" || identity.GetSiteUid() == "" || identity.GetUserId() <= 0 || identity.GetRuntimeUuid() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "workspace session identity is incomplete")
	}
	_, site, err := s.runtimeForGateway(ctx, component, identity.RuntimeUuid)
	if err != nil {
		if apierrors.IsNotFound(err) || connect.CodeOf(err) == connect.CodeNotFound {
			return &componentv1.LookupWorkspaceSessionResponse{}, nil
		}
		return nil, err
	}
	if string(site.UID) != identity.SiteUid {
		return &componentv1.LookupWorkspaceSessionResponse{}, nil
	}
	renewable, err := s.workspaceSessions.lookup(component.UID, identity, time.Now())
	if err != nil {
		return nil, err
	}
	return &componentv1.LookupWorkspaceSessionResponse{Renewable: renewable}, nil
}
