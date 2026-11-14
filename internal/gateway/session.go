// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

const gatewaySessionConnectTimeout = 30 * time.Second

var (
	errGatewaySessionAmbiguous    = errors.New("gateway session candidates are ambiguous")
	errGatewaySessionLimitReached = errors.New("gateway session limit reached")
)

type sessionRegistry struct {
	mu                      sync.Mutex
	ttl                     time.Duration
	idleTimeout             time.Duration
	maxSessionsPerCodespace int
	maxSessionsPerUser      int
	tickets                 *gatewaySessionTickets
	nextLiveID              int64
	liveEntries             map[string]map[int64]gatewayLiveSession
	sessionCountByCodespace map[string]int
	sessionCountByUser      map[gatewayUserKey]int
	sessions                map[string]*gatewaySession
	activityChanges         chan struct{}
	workspaceRevision       uint64
	acknowledgedRevision    uint64
	activityAcknowledged    chan struct{}
	requireWorkspaceAck     bool
}

type gatewayActivitySnapshot struct {
	runtimes   map[string]int64
	workspaces []*componentv1.WorkspaceSessionActivity
	revision   uint64
}

// Activity reports connection counts and Workspace sessions from the same snapshot.
func (r *sessionRegistry) Activity(now time.Time) gatewayActivitySnapshot {
	r.mu.Lock()
	cancels := r.dropExpiredSessionsLocked(now)
	activity := gatewayActivitySnapshot{runtimes: make(map[string]int64, len(r.liveEntries)), revision: r.workspaceRevision}
	for id, entries := range r.liveEntries {
		activity.runtimes[id] = int64(len(entries))
	}
	for _, session := range r.sessions {
		if session.endpointID != runtimeendpoint.WorkspaceEndpointID || !session.established {
			continue
		}
		activity.workspaces = append(activity.workspaces, &componentv1.WorkspaceSessionActivity{
			Identity: &componentv1.WorkspaceSessionIdentity{SessionId: session.id, SiteUid: session.siteUID, UserId: session.userID, RuntimeUuid: session.codespaceUUID},
			Active:   session.activeRequests > 0, LastActiveUnixMilliseconds: session.lastActive.UnixMilli(),
		})
	}
	r.mu.Unlock()
	cancelGatewaySessions(cancels)
	return activity
}

type gatewaySession struct {
	id               string
	siteUID          string
	userID           int64
	codespaceUUID    string
	endpointID       string
	created          time.Time
	lastActive       time.Time
	established      bool
	expires          time.Time
	token            string
	activeRequests   int
	activityRevision uint64
}

type gatewayLiveSession struct {
	cancel        context.CancelFunc
	sessionID     string
	siteUID       string
	userID        int64
	countsSession bool
}

type gatewayUserKey struct {
	siteUID string
	userID  int64
}

func newSessionRegistryFromConfig(config configpkg.GatewayConfig, sessionKey []byte) (*sessionRegistry, error) {
	tickets, err := newGatewaySessionTickets(sessionKey)
	if err != nil {
		return nil, err
	}
	ttl := config.Sessions.TTL.ToStdlib()
	idleTimeout := config.Sessions.IdleTimeout.ToStdlib()
	maxSessionsPerCodespace := config.Sessions.MaxPerCodespace
	maxSessionsPerUser := config.Sessions.MaxPerUser
	if ttl <= 0 || idleTimeout <= 0 || maxSessionsPerCodespace <= 0 || maxSessionsPerUser <= 0 {
		return nil, fmt.Errorf("gateway session configuration is incomplete")
	}
	return &sessionRegistry{
		ttl:                     ttl,
		idleTimeout:             idleTimeout,
		maxSessionsPerCodespace: maxSessionsPerCodespace,
		maxSessionsPerUser:      maxSessionsPerUser,
		tickets:                 tickets,
		liveEntries:             make(map[string]map[int64]gatewayLiveSession),
		sessionCountByCodespace: make(map[string]int),
		sessionCountByUser:      make(map[gatewayUserKey]int),
		sessions:                make(map[string]*gatewaySession),
		activityChanges:         make(chan struct{}, 1),
		activityAcknowledged:    make(chan struct{}),
	}, nil
}

func (r *sessionRegistry) ActivityChanges() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.activityChanges
}

func (r *sessionRegistry) CreateReplacingAny(binding openTokenBinding, replaceIDs []string, now time.Time) (string, error) {
	if binding.SiteUID == "" || binding.UserID <= 0 || binding.CodespaceUUID == "" || binding.EndpointID == "" {
		return "", fmt.Errorf("gateway session binding is incomplete")
	}
	id, err := newGatewaySessionID()
	if err != nil {
		return "", err
	}
	expires := now.Add(r.ttl)
	session := &gatewaySession{
		id: id, siteUID: binding.SiteUID, userID: binding.UserID, codespaceUUID: binding.CodespaceUUID, endpointID: binding.EndpointID,
		created: now, lastActive: now, expires: expires,
	}
	token, err := r.issueTicket(session, now)
	if err != nil {
		return "", err
	}
	session.token = token
	r.mu.Lock()

	cancels := r.dropExpiredSessionsLocked(now)
	replaceID, ambiguous := r.matchingSessionIDLocked(replaceIDs, binding, now)
	if ambiguous {
		r.mu.Unlock()
		cancelGatewaySessions(cancels)
		return "", errGatewaySessionAmbiguous
	}
	if replaceID != "" {
		cancels = append(cancels, r.deleteGatewaySessionAndLiveLocked(replaceID)...)
	}
	if !r.canAddSessionLocked(binding.SiteUID, binding.UserID, binding.CodespaceUUID) {
		r.mu.Unlock()
		cancelGatewaySessions(cancels)
		return "", errGatewaySessionLimitReached
	}
	r.sessions[id] = session
	r.incrementSessionCountLocked(binding.SiteUID, binding.UserID, binding.CodespaceUUID)
	r.mu.Unlock()
	cancelGatewaySessions(cancels)
	return token, nil
}

func (r *sessionRegistry) ResolveAny(ctx context.Context, ids []string, codespaceUUID, endpointID string, now time.Time, lookup func(context.Context, *componentv1.WorkspaceSessionIdentity) (bool, error)) (gatewaySession, bool, bool, error) {
	if len(ids) == 0 || codespaceUUID == "" || endpointID == "" {
		return gatewaySession{}, false, false, nil
	}
	var matched *gatewaySessionTicket
	for _, token := range ids {
		ticket, ok := r.tickets.open(token)
		if !ok || ticket.CodespaceUUID != codespaceUUID || ticket.EndpointID != endpointID {
			continue
		}
		valid := ticket.validAt(now)
		if !valid && endpointID == runtimeendpoint.WorkspaceEndpointID {
			r.mu.Lock()
			local := r.sessions[ticket.ID]
			valid = local != nil && local.established && local.siteUID == ticket.SiteUID && local.userID == ticket.UserID && local.codespaceUUID == codespaceUUID && local.endpointID == endpointID && !r.sessionExpiredLocked(local, now)
			r.mu.Unlock()
			if !valid && lookup != nil {
				var err error
				valid, err = lookup(ctx, &componentv1.WorkspaceSessionIdentity{SessionId: ticket.ID, SiteUid: ticket.SiteUID, UserId: ticket.UserID, RuntimeUuid: codespaceUUID})
				if err != nil {
					return gatewaySession{}, false, false, err
				}
			}
		}
		if !valid {
			continue
		}
		if matched != nil && matched.ID != ticket.ID {
			return gatewaySession{}, false, true, nil
		}
		copyTicket := ticket
		matched = &copyTicket
	}
	if matched == nil {
		return gatewaySession{}, false, false, nil
	}
	return gatewaySession{
		id: matched.ID, siteUID: matched.SiteUID, userID: matched.UserID, codespaceUUID: matched.CodespaceUUID, endpointID: matched.EndpointID,
		created: time.UnixMilli(matched.IssuedAt), expires: time.UnixMilli(matched.ExpiresAt),
	}, true, false, nil
}

// Accept commits activity only after the caller has checked authorization and routing.
func (r *sessionRegistry) Accept(candidate gatewaySession, now time.Time) (gatewaySession, error) {
	r.mu.Lock()
	cancels := r.dropExpiredSessionsLocked(now)
	defer func() {
		r.mu.Unlock()
		cancelGatewaySessions(cancels)
	}()
	session := r.sessions[candidate.id]
	if session == nil {
		if !r.canAddSessionLocked(candidate.siteUID, candidate.userID, candidate.codespaceUUID) {
			return gatewaySession{}, errGatewaySessionLimitReached
		}
	} else if session.siteUID != candidate.siteUID || session.userID != candidate.userID || session.codespaceUUID != candidate.codespaceUUID || session.endpointID != candidate.endpointID {
		return gatewaySession{}, fmt.Errorf("gateway session identity changed")
	} else {
		candidate = *session
	}
	candidate.established = true
	candidate.lastActive = now
	if candidate.endpointID == runtimeendpoint.WorkspaceEndpointID {
		candidate.expires = now.Add(r.ttl)
	}
	token, err := r.issueTicket(&candidate, now)
	if err != nil {
		return gatewaySession{}, err
	}
	candidate.token = token
	if session == nil {
		r.incrementSessionCountLocked(candidate.siteUID, candidate.userID, candidate.codespaceUUID)
	}
	r.sessions[candidate.id] = &candidate
	return candidate, nil
}

func (r *sessionRegistry) BeginSessionCancelable(sessionID, codespaceUUID string, cancel context.CancelFunc) func() {
	return r.beginLive(codespaceUUID, gatewayLiveSession{cancel: cancel, sessionID: sessionID}, time.Time{})
}

func (r *sessionRegistry) BeginSSHSession(siteUID, codespaceUUID string, userID int64, cancel context.CancelFunc, now time.Time) (func(), bool) {
	if siteUID == "" || codespaceUUID == "" || userID <= 0 {
		return func() {}, false
	}
	release := r.beginLive(codespaceUUID, gatewayLiveSession{
		cancel: cancel, siteUID: siteUID, userID: userID, countsSession: true,
	}, now)
	if release == nil {
		return func() {}, false
	}
	return release, true
}

func (r *sessionRegistry) beginLive(codespaceUUID string, entry gatewayLiveSession, now time.Time) func() {
	if codespaceUUID == "" {
		return func() {}
	}
	r.mu.Lock()
	var cancels []context.CancelFunc
	if entry.countsSession {
		cancels = r.dropExpiredSessionsLocked(now)
		if !r.canAddSessionLocked(entry.siteUID, entry.userID, codespaceUUID) {
			r.mu.Unlock()
			cancelGatewaySessions(cancels)
			return nil
		}
		r.incrementSessionCountLocked(entry.siteUID, entry.userID, codespaceUUID)
	}
	r.nextLiveID++
	liveID := r.nextLiveID
	becameActive := len(r.liveEntries[codespaceUUID]) == 0
	if r.liveEntries[codespaceUUID] == nil {
		r.liveEntries[codespaceUUID] = make(map[int64]gatewayLiveSession)
	}
	r.liveEntries[codespaceUUID][liveID] = entry
	workspaceChanged := false
	if session := r.sessions[entry.sessionID]; session != nil {
		workspaceChanged = session.endpointID == runtimeendpoint.WorkspaceEndpointID && session.activeRequests == 0
		session.activeRequests++
		if workspaceChanged {
			r.workspaceRevision++
			session.activityRevision = r.workspaceRevision
		}
	}
	r.mu.Unlock()
	cancelGatewaySessions(cancels)
	if becameActive || workspaceChanged {
		r.signalActivityChange()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			current, ok := r.liveEntries[codespaceUUID][liveID]
			if !ok {
				r.mu.Unlock()
				return
			}
			if current.countsSession {
				r.decrementSessionCountLocked(current.siteUID, current.userID, codespaceUUID)
			}
			if session := r.sessions[current.sessionID]; session != nil {
				session.lastActive = time.Now()
				session.activeRequests--
				workspaceChanged = session.endpointID == runtimeendpoint.WorkspaceEndpointID && session.activeRequests == 0
				if workspaceChanged {
					r.workspaceRevision++
				}
			} else {
				workspaceChanged = false
			}
			delete(r.liveEntries[codespaceUUID], liveID)
			becameIdle := len(r.liveEntries[codespaceUUID]) == 0
			if becameIdle {
				delete(r.liveEntries, codespaceUUID)
			}
			r.mu.Unlock()
			if becameIdle || workspaceChanged {
				r.signalActivityChange()
			}
		})
	}
}

func (r *sessionRegistry) signalActivityChange() {
	select {
	case r.activityChanges <- struct{}{}:
	default:
	}
}

func (r *sessionRegistry) canAddSessionLocked(siteUID string, userID int64, codespaceUUID string) bool {
	return r.sessionCountByCodespace[codespaceUUID] < r.maxSessionsPerCodespace &&
		r.sessionCountByUser[gatewayUserKey{siteUID: siteUID, userID: userID}] < r.maxSessionsPerUser
}

func (r *sessionRegistry) deleteGatewaySessionAndLiveLocked(id string) []context.CancelFunc {
	session := r.sessions[id]
	if session == nil {
		return nil
	}
	delete(r.sessions, id)
	r.decrementSessionCountLocked(session.siteUID, session.userID, session.codespaceUUID)
	var cancels []context.CancelFunc
	for liveID, entry := range r.liveEntries[session.codespaceUUID] {
		if entry.sessionID != id {
			continue
		}
		if entry.cancel != nil {
			cancels = append(cancels, entry.cancel)
		}
		delete(r.liveEntries[session.codespaceUUID], liveID)
	}
	if len(r.liveEntries[session.codespaceUUID]) == 0 {
		delete(r.liveEntries, session.codespaceUUID)
	}
	return cancels
}

func (r *sessionRegistry) matchingSessionIDLocked(ids []string, binding openTokenBinding, now time.Time) (string, bool) {
	seen := make(map[string]struct{}, len(ids))
	matchedID := ""
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ticket, ok := r.tickets.open(id)
		if !ok || ticket.CodespaceUUID != binding.CodespaceUUID || ticket.EndpointID != binding.EndpointID {
			continue
		}
		if !ticket.validAt(now) {
			local := r.sessions[ticket.ID]
			if binding.EndpointID != runtimeendpoint.WorkspaceEndpointID || local == nil || r.sessionExpiredLocked(local, now) {
				continue
			}
		}
		if binding.UserID > 0 && ticket.UserID != binding.UserID {
			continue
		}
		if binding.SiteUID != "" && ticket.SiteUID != binding.SiteUID {
			continue
		}
		if matchedID != "" && matchedID != ticket.ID {
			return "", true
		}
		matchedID = ticket.ID
	}
	return matchedID, false
}

func (r *sessionRegistry) incrementSessionCountLocked(siteUID string, userID int64, codespaceUUID string) {
	r.sessionCountByCodespace[codespaceUUID]++
	r.sessionCountByUser[gatewayUserKey{siteUID: siteUID, userID: userID}]++
}

func (r *sessionRegistry) decrementSessionCountLocked(siteUID string, userID int64, codespaceUUID string) {
	if current := r.sessionCountByCodespace[codespaceUUID]; current <= 1 {
		delete(r.sessionCountByCodespace, codespaceUUID)
	} else {
		r.sessionCountByCodespace[codespaceUUID] = current - 1
	}
	user := gatewayUserKey{siteUID: siteUID, userID: userID}
	if current := r.sessionCountByUser[user]; current <= 1 {
		delete(r.sessionCountByUser, user)
	} else {
		r.sessionCountByUser[user] = current - 1
	}
}

func (r *sessionRegistry) dropExpiredSessionsLocked(now time.Time) []context.CancelFunc {
	if now.IsZero() {
		now = time.Now()
	}
	var cancels []context.CancelFunc
	for id, session := range r.sessions {
		if r.sessionExpiredLocked(session, now) {
			cancels = append(cancels, r.deleteGatewaySessionAndLiveLocked(id)...)
		}
	}
	return cancels
}

func (r *sessionRegistry) sessionExpiredLocked(session *gatewaySession, now time.Time) bool {
	if session == nil {
		return true
	}
	if !session.established && now.Sub(session.created) > gatewaySessionConnectTimeout {
		return true
	}
	if session.endpointID != runtimeendpoint.WorkspaceEndpointID && !session.expires.IsZero() && !now.Before(session.expires) {
		return true
	}
	if !session.established || r.idleTimeout <= 0 || now.Sub(session.lastActive) < r.idleTimeout {
		return false
	}
	return session.activeRequests == 0
}

func (r *sessionRegistry) acknowledgeWorkspaceActivity(revision uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if revision > r.acknowledgedRevision {
		r.acknowledgedRevision = revision
		close(r.activityAcknowledged)
		r.activityAcknowledged = make(chan struct{})
	}
}

func (r *sessionRegistry) waitWorkspaceActivity(ctx context.Context, sessionID string) error {
	if !r.requireWorkspaceAck {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayAuthorizationTimeout)
	defer cancel()
	for {
		r.mu.Lock()
		session := r.sessions[sessionID]
		if session == nil {
			r.mu.Unlock()
			return fmt.Errorf("gateway session is no longer available")
		}
		ready := session.endpointID != runtimeendpoint.WorkspaceEndpointID || session.activityRevision <= r.acknowledgedRevision
		changed := r.activityAcknowledged
		r.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (r *sessionRegistry) issueTicket(session *gatewaySession, now time.Time) (string, error) {
	idleExpires := session.expires
	if !session.established && now.Add(gatewaySessionConnectTimeout).Before(idleExpires) {
		idleExpires = now.Add(gatewaySessionConnectTimeout)
	}
	if r.idleTimeout > 0 && now.Add(r.idleTimeout).Before(idleExpires) {
		idleExpires = now.Add(r.idleTimeout)
	}
	return r.tickets.seal(gatewaySessionTicket{
		Version: gatewaySessionTicketVersion, ID: session.id, SiteUID: session.siteUID, UserID: session.userID,
		CodespaceUUID: session.codespaceUUID, EndpointID: session.endpointID,
		IssuedAt: session.created.UnixMilli(), ExpiresAt: session.expires.UnixMilli(), IdleExpiresAt: idleExpires.UnixMilli(),
	})
}

func cancelGatewaySessions(cancels []context.CancelFunc) {
	for _, cancel := range cancels {
		cancel()
	}
}

func newGatewaySessionID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate gateway session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
