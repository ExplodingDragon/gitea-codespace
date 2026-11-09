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
	live                    map[string]int
	nextLiveID              int64
	liveEntries             map[string]map[int64]gatewayLiveSession
	sessionCountByCodespace map[string]int
	sessionCountByUser      map[gatewayUserKey]int
	sessions                map[string]*gatewaySession
	activityChanges         chan struct{}
}

// Activity expires idle sessions before reporting the current connection counts.
func (r *sessionRegistry) Activity(now time.Time) map[string]int {
	r.mu.Lock()
	cancels := r.dropExpiredSessionsLocked(now)
	activity := make(map[string]int, len(r.live))
	for id, count := range r.live {
		if count > 0 {
			activity[id] = count
		}
	}
	r.mu.Unlock()
	cancelGatewaySessions(cancels)
	return activity
}

type gatewaySession struct {
	id            string
	siteUID       string
	userID        int64
	codespaceUUID string
	endpointID    string
	created       time.Time
	lastActive    time.Time
	established   bool
	expires       time.Time
	token         string
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
		live:                    make(map[string]int),
		liveEntries:             make(map[string]map[int64]gatewayLiveSession),
		sessionCountByCodespace: make(map[string]int),
		sessionCountByUser:      make(map[gatewayUserKey]int),
		sessions:                make(map[string]*gatewaySession),
		activityChanges:         make(chan struct{}, 1),
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

func (r *sessionRegistry) AuthenticateAny(ids []string, codespaceUUID, endpointID string, now time.Time) (gatewaySession, bool, bool) {
	if len(ids) == 0 || codespaceUUID == "" || endpointID == "" {
		return gatewaySession{}, false, false
	}
	var matched *gatewaySessionTicket
	for _, token := range ids {
		ticket, ok := r.tickets.open(token, now)
		if !ok || ticket.CodespaceUUID != codespaceUUID || ticket.EndpointID != endpointID {
			continue
		}
		if matched != nil && matched.ID != ticket.ID {
			return gatewaySession{}, false, true
		}
		copyTicket := ticket
		matched = &copyTicket
	}
	if matched == nil {
		return gatewaySession{}, false, false
	}
	r.mu.Lock()
	cancels := r.dropExpiredSessionsLocked(now)
	session := r.sessions[matched.ID]
	if session == nil {
		if !r.canAddSessionLocked(matched.SiteUID, matched.UserID, matched.CodespaceUUID) {
			r.mu.Unlock()
			cancelGatewaySessions(cancels)
			return gatewaySession{}, false, false
		}
		session = &gatewaySession{
			id: matched.ID, siteUID: matched.SiteUID, userID: matched.UserID, codespaceUUID: matched.CodespaceUUID, endpointID: matched.EndpointID,
			created: time.UnixMilli(matched.IssuedAt), expires: time.UnixMilli(matched.ExpiresAt), established: true,
		}
		r.sessions[session.id] = session
		r.incrementSessionCountLocked(session.siteUID, session.userID, session.codespaceUUID)
	} else if session.siteUID != matched.SiteUID || session.userID != matched.UserID || session.codespaceUUID != matched.CodespaceUUID || session.endpointID != matched.EndpointID {
		r.mu.Unlock()
		cancelGatewaySessions(cancels)
		return gatewaySession{}, false, false
	}
	session.established = true
	session.lastActive = now
	token, err := r.issueTicket(session, now)
	if err != nil {
		r.mu.Unlock()
		cancelGatewaySessions(cancels)
		return gatewaySession{}, false, false
	}
	session.token = token
	result := *session
	r.mu.Unlock()
	cancelGatewaySessions(cancels)
	return result, true, false
}

func (r *sessionRegistry) BeginSessionCancelable(sessionID, codespaceUUID string, cancel context.CancelFunc) func() {
	if ticket, ok := r.tickets.open(sessionID, time.Now()); ok {
		sessionID = ticket.ID
	}
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
	becameActive := r.live[codespaceUUID] == 0
	r.live[codespaceUUID]++
	if r.liveEntries[codespaceUUID] == nil {
		r.liveEntries[codespaceUUID] = make(map[int64]gatewayLiveSession)
	}
	r.liveEntries[codespaceUUID][liveID] = entry
	r.mu.Unlock()
	cancelGatewaySessions(cancels)
	if becameActive {
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
			}
			delete(r.liveEntries[codespaceUUID], liveID)
			if len(r.liveEntries[codespaceUUID]) == 0 {
				delete(r.liveEntries, codespaceUUID)
			}
			becameIdle := r.live[codespaceUUID] == 1
			r.decrementLiveLocked(codespaceUUID)
			r.mu.Unlock()
			if becameIdle {
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

func (r *sessionRegistry) DeleteCodespace(codespaceUUID string) int {
	if r == nil || codespaceUUID == "" {
		return 0
	}
	r.mu.Lock()

	deleted := 0
	for id, session := range r.sessions {
		if session.codespaceUUID == codespaceUUID {
			r.deleteGatewaySessionLocked(id)
			deleted++
		}
	}
	var cancels []context.CancelFunc
	for liveID, entry := range r.liveEntries[codespaceUUID] {
		if entry.countsSession {
			r.decrementSessionCountLocked(entry.siteUID, entry.userID, codespaceUUID)
		}
		if entry.cancel != nil {
			cancels = append(cancels, entry.cancel)
		}
		delete(r.liveEntries[codespaceUUID], liveID)
	}
	delete(r.liveEntries, codespaceUUID)
	delete(r.live, codespaceUUID)
	r.mu.Unlock()

	cancelGatewaySessions(cancels)
	return deleted
}

func (r *sessionRegistry) canAddSessionLocked(siteUID string, userID int64, codespaceUUID string) bool {
	return r.sessionCountByCodespace[codespaceUUID] < r.maxSessionsPerCodespace &&
		r.sessionCountByUser[gatewayUserKey{siteUID: siteUID, userID: userID}] < r.maxSessionsPerUser
}

func (r *sessionRegistry) deleteLiveEntriesLocked(codespaceUUID, sessionID string) []context.CancelFunc {
	if codespaceUUID == "" || sessionID == "" {
		return nil
	}
	var cancels []context.CancelFunc
	for liveID, entry := range r.liveEntries[codespaceUUID] {
		if entry.sessionID != sessionID {
			continue
		}
		if entry.cancel != nil {
			cancels = append(cancels, entry.cancel)
		}
		delete(r.liveEntries[codespaceUUID], liveID)
		r.decrementLiveLocked(codespaceUUID)
	}
	if len(r.liveEntries[codespaceUUID]) == 0 {
		delete(r.liveEntries, codespaceUUID)
	}
	return cancels
}

func (r *sessionRegistry) deleteGatewaySessionAndLiveLocked(id string) []context.CancelFunc {
	session := r.sessions[id]
	if session == nil {
		return nil
	}
	r.deleteGatewaySessionLocked(id)
	return r.deleteLiveEntriesLocked(session.codespaceUUID, id)
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
		ticket, ok := r.tickets.open(id, now)
		if !ok || ticket.CodespaceUUID != binding.CodespaceUUID || ticket.EndpointID != binding.EndpointID {
			continue
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

func (r *sessionRegistry) decrementLiveLocked(codespaceUUID string) {
	current := r.live[codespaceUUID]
	if current <= 1 {
		delete(r.live, codespaceUUID)
		return
	}
	r.live[codespaceUUID] = current - 1
}

func (r *sessionRegistry) deleteGatewaySessionLocked(id string) {
	session := r.sessions[id]
	if session == nil {
		return
	}
	delete(r.sessions, id)
	r.decrementSessionCountLocked(session.siteUID, session.userID, session.codespaceUUID)
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
	if !session.expires.IsZero() && now.After(session.expires) {
		return true
	}
	if !session.established || r.idleTimeout <= 0 || now.Sub(session.lastActive) <= r.idleTimeout {
		return false
	}
	for _, entry := range r.liveEntries[session.codespaceUUID] {
		if entry.sessionID == session.id {
			return false
		}
	}
	return true
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
