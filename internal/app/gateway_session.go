// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

const gatewaySessionConnectTimeout = 30 * time.Second

var (
	errGatewaySessionAmbiguous    = errors.New("gateway session candidates are ambiguous")
	errGatewaySessionLimitReached = errors.New("gateway session limit reached")
)

type gatewaySessionRegistry struct {
	mu                      sync.Mutex
	ttl                     time.Duration
	idleTimeout             time.Duration
	maxSessionsPerCodespace int
	maxSessionsPerUser      int
	anonymousLive           map[string]int
	live                    map[string]int
	nextLiveID              int64
	liveEntries             map[string]map[int64]gatewayLiveSession
	sessionCountByCodespace map[string]int
	sessionCountByUser      map[int64]int
	sessions                map[string]*gatewaySession
}

type gatewaySession struct {
	id            string
	userID        int64
	codespaceUUID string
	endpointID    string
	created       time.Time
	lastActive    time.Time
	established   bool
}

type gatewayLiveSession struct {
	cancel        context.CancelFunc
	sessionID     string
	userID        int64
	countsSession bool
}

func newGatewaySessionRegistryFromConfig(config GatewayConfig) *gatewaySessionRegistry {
	ttl := config.Sessions.TTL.ToStdlib()
	if ttl <= 0 {
		ttl = DefaultConfig().Gateway.Sessions.TTL.ToStdlib()
	}
	idleTimeout := config.Sessions.IdleTimeout.ToStdlib()
	if idleTimeout <= 0 {
		idleTimeout = DefaultConfig().Gateway.Sessions.IdleTimeout.ToStdlib()
	}
	maxSessionsPerCodespace := config.Sessions.MaxPerCodespace
	if maxSessionsPerCodespace <= 0 {
		maxSessionsPerCodespace = DefaultConfig().Gateway.Sessions.MaxPerCodespace
	}
	maxSessionsPerUser := config.Sessions.MaxPerUser
	if maxSessionsPerUser <= 0 {
		maxSessionsPerUser = DefaultConfig().Gateway.Sessions.MaxPerUser
	}
	return &gatewaySessionRegistry{
		ttl:                     ttl,
		idleTimeout:             idleTimeout,
		maxSessionsPerCodespace: maxSessionsPerCodespace,
		maxSessionsPerUser:      maxSessionsPerUser,
		anonymousLive:           make(map[string]int),
		live:                    make(map[string]int),
		liveEntries:             make(map[string]map[int64]gatewayLiveSession),
		sessionCountByCodespace: make(map[string]int),
		sessionCountByUser:      make(map[int64]int),
		sessions:                make(map[string]*gatewaySession),
	}
}

func (r *gatewaySessionRegistry) Create(binding gatewayOpenTokenBinding, now time.Time) (string, error) {
	return r.CreateReplacingAny(binding, nil, now)
}

func (r *gatewaySessionRegistry) CreateReplacing(binding gatewayOpenTokenBinding, replaceID string, now time.Time) (string, error) {
	var replaceIDs []string
	if replaceID != "" {
		replaceIDs = []string{replaceID}
	}
	return r.CreateReplacingAny(binding, replaceIDs, now)
}

func (r *gatewaySessionRegistry) CreateReplacingAny(binding gatewayOpenTokenBinding, replaceIDs []string, now time.Time) (string, error) {
	if binding.userID <= 0 || binding.codespaceUUID == "" || binding.endpointID == "" {
		return "", fmt.Errorf("gateway session binding is incomplete")
	}
	id, err := newGatewaySessionID()
	if err != nil {
		return "", err
	}
	r.mu.Lock()

	cancels := r.dropExpiredSessionsLocked(now)
	replaceID, ambiguous := r.matchingSessionIDLocked(replaceIDs, binding)
	if ambiguous {
		r.mu.Unlock()
		cancelGatewaySessions(cancels)
		return "", errGatewaySessionAmbiguous
	}
	if replaceID != "" {
		cancels = append(cancels, r.deleteGatewaySessionAndLiveLocked(replaceID)...)
	}
	if !r.canAddSessionLocked(binding.userID, binding.codespaceUUID) {
		r.mu.Unlock()
		cancelGatewaySessions(cancels)
		return "", errGatewaySessionLimitReached
	}
	r.sessions[id] = &gatewaySession{
		id:            id,
		userID:        binding.userID,
		codespaceUUID: binding.codespaceUUID,
		endpointID:    binding.endpointID,
		created:       now,
		lastActive:    now,
		established:   false,
	}
	r.incrementSessionCountLocked(binding.userID, binding.codespaceUUID)
	r.mu.Unlock()
	cancelGatewaySessions(cancels)
	return id, nil
}

func (r *gatewaySessionRegistry) Authenticate(id, codespaceUUID, endpointID string, now time.Time) (gatewaySession, bool) {
	session, ok, ambiguous := r.AuthenticateAny([]string{id}, codespaceUUID, endpointID, now)
	if !ok || ambiguous {
		return gatewaySession{}, false
	}
	return session, true
}

func (r *gatewaySessionRegistry) AuthenticateAny(ids []string, codespaceUUID, endpointID string, now time.Time) (gatewaySession, bool, bool) {
	if len(ids) == 0 || codespaceUUID == "" || endpointID == "" {
		return gatewaySession{}, false, false
	}
	r.mu.Lock()

	cancels := r.dropExpiredSessionsLocked(now)
	binding := gatewayOpenTokenBinding{
		codespaceUUID: codespaceUUID,
		endpointID:    endpointID,
	}
	id, ambiguous := r.matchingSessionIDLocked(ids, binding)
	if ambiguous {
		r.mu.Unlock()
		cancelGatewaySessions(cancels)
		return gatewaySession{}, false, true
	}
	session := r.sessions[id]
	if session == nil {
		r.mu.Unlock()
		cancelGatewaySessions(cancels)
		return gatewaySession{}, false, false
	}
	session.established = true
	session.lastActive = now
	result := *session
	r.mu.Unlock()
	cancelGatewaySessions(cancels)
	return result, true, false
}

func (r *gatewaySessionRegistry) Begin(codespaceUUID string) func() {
	return r.BeginCancelable(codespaceUUID, nil)
}

func (r *gatewaySessionRegistry) BeginCancelable(codespaceUUID string, cancel context.CancelFunc) func() {
	return r.beginLive(codespaceUUID, "", 0, cancel, false, time.Time{})
}

func (r *gatewaySessionRegistry) BeginSessionCancelable(sessionID, codespaceUUID string, cancel context.CancelFunc) func() {
	return r.beginLive(codespaceUUID, sessionID, 0, cancel, false, time.Time{})
}

func (r *gatewaySessionRegistry) BeginSSHSession(codespaceUUID string, userID int64, cancel context.CancelFunc, now time.Time) (func(), bool) {
	if codespaceUUID == "" || userID <= 0 {
		return func() {}, false
	}
	release := r.beginLive(codespaceUUID, "", userID, cancel, true, now)
	if release == nil {
		return func() {}, false
	}
	return release, true
}

func (r *gatewaySessionRegistry) beginLive(codespaceUUID, sessionID string, userID int64, cancel context.CancelFunc, countsSession bool, now time.Time) func() {
	if codespaceUUID == "" {
		return func() {}
	}
	r.mu.Lock()
	var cancels []context.CancelFunc
	if countsSession {
		cancels = r.dropExpiredSessionsLocked(now)
		if !r.canAddSessionLocked(userID, codespaceUUID) {
			r.mu.Unlock()
			cancelGatewaySessions(cancels)
			return nil
		}
		r.incrementSessionCountLocked(userID, codespaceUUID)
	} else if sessionID == "" {
		r.anonymousLive[codespaceUUID]++
	}
	r.nextLiveID++
	liveID := r.nextLiveID
	r.live[codespaceUUID]++
	tracked := cancel != nil || countsSession || sessionID != ""
	if tracked {
		if r.liveEntries[codespaceUUID] == nil {
			r.liveEntries[codespaceUUID] = make(map[int64]gatewayLiveSession)
		}
		r.liveEntries[codespaceUUID][liveID] = gatewayLiveSession{
			cancel:        cancel,
			sessionID:     sessionID,
			userID:        userID,
			countsSession: countsSession,
		}
	}
	r.mu.Unlock()
	cancelGatewaySessions(cancels)

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()

			entry, hadEntry := r.liveEntries[codespaceUUID][liveID]
			if tracked && !hadEntry {
				return
			}
			if hadEntry {
				if entry.countsSession {
					r.decrementSessionCountLocked(entry.userID, codespaceUUID)
				}
				delete(r.liveEntries[codespaceUUID], liveID)
				if len(r.liveEntries[codespaceUUID]) == 0 {
					delete(r.liveEntries, codespaceUUID)
				}
			}
			if !countsSession && sessionID == "" {
				r.decrementAnonymousLiveLocked(codespaceUUID)
			}
			current := r.live[codespaceUUID]
			if current <= 1 {
				delete(r.live, codespaceUUID)
				return
			}
			r.live[codespaceUUID] = current - 1
		})
	}
}

func (r *gatewaySessionRegistry) LiveSessions(codespaceUUID string) int {
	if r == nil || codespaceUUID == "" {
		return 0
	}
	return r.liveSessionsAt(codespaceUUID, time.Now())
}

func (r *gatewaySessionRegistry) liveSessionsAt(codespaceUUID string, now time.Time) int {
	if r == nil || codespaceUUID == "" {
		return 0
	}
	r.mu.Lock()
	cancels := r.dropExpiredSessionsLocked(now)
	live := r.sessionCountByCodespace[codespaceUUID] + r.anonymousLive[codespaceUUID]
	r.mu.Unlock()
	cancelGatewaySessions(cancels)
	return live
}

func (r *gatewaySessionRegistry) DeleteEndpoint(codespaceUUID, endpointID string) int {
	if r == nil || codespaceUUID == "" || endpointID == "" {
		return 0
	}
	r.mu.Lock()

	deleted := 0
	deletedIDs := make(map[string]struct{})
	for id, session := range r.sessions {
		if session.codespaceUUID == codespaceUUID && session.endpointID == endpointID {
			r.deleteGatewaySessionLocked(id)
			deletedIDs[id] = struct{}{}
			deleted++
		}
	}
	var cancels []context.CancelFunc
	for id := range deletedIDs {
		cancels = append(cancels, r.deleteLiveEntriesLocked(codespaceUUID, id)...)
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return deleted
}

func (r *gatewaySessionRegistry) DeleteCodespace(codespaceUUID string) int {
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
			r.decrementSessionCountLocked(entry.userID, codespaceUUID)
		}
		if entry.cancel != nil {
			cancels = append(cancels, entry.cancel)
		}
		delete(r.liveEntries[codespaceUUID], liveID)
	}
	delete(r.liveEntries, codespaceUUID)
	delete(r.live, codespaceUUID)
	delete(r.anonymousLive, codespaceUUID)
	r.mu.Unlock()

	cancelGatewaySessions(cancels)
	return deleted
}

func (r *gatewaySessionRegistry) canAddSessionLocked(userID int64, codespaceUUID string) bool {
	return r.sessionCountByCodespace[codespaceUUID] < r.maxSessionsPerCodespace &&
		r.sessionCountByUser[userID] < r.maxSessionsPerUser
}

func (r *gatewaySessionRegistry) deleteLiveEntriesLocked(codespaceUUID, sessionID string) []context.CancelFunc {
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
		if !entry.countsSession && entry.sessionID == "" {
			r.decrementAnonymousLiveLocked(codespaceUUID)
		}
		delete(r.liveEntries[codespaceUUID], liveID)
		r.decrementLiveLocked(codespaceUUID)
	}
	if len(r.liveEntries[codespaceUUID]) == 0 {
		delete(r.liveEntries, codespaceUUID)
	}
	return cancels
}

func (r *gatewaySessionRegistry) deleteGatewaySessionAndLiveLocked(id string) []context.CancelFunc {
	session := r.sessions[id]
	if session == nil {
		return nil
	}
	r.deleteGatewaySessionLocked(id)
	return r.deleteLiveEntriesLocked(session.codespaceUUID, id)
}

func (r *gatewaySessionRegistry) matchingSessionIDLocked(ids []string, binding gatewayOpenTokenBinding) (string, bool) {
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
		session := r.sessions[id]
		if session == nil ||
			session.codespaceUUID != binding.codespaceUUID ||
			session.endpointID != binding.endpointID {
			continue
		}
		if binding.userID > 0 && session.userID != binding.userID {
			continue
		}
		if matchedID != "" {
			return "", true
		}
		matchedID = id
	}
	return matchedID, false
}

func (r *gatewaySessionRegistry) incrementSessionCountLocked(userID int64, codespaceUUID string) {
	r.sessionCountByCodespace[codespaceUUID]++
	r.sessionCountByUser[userID]++
}

func (r *gatewaySessionRegistry) decrementSessionCountLocked(userID int64, codespaceUUID string) {
	if current := r.sessionCountByCodespace[codespaceUUID]; current <= 1 {
		delete(r.sessionCountByCodespace, codespaceUUID)
	} else {
		r.sessionCountByCodespace[codespaceUUID] = current - 1
	}
	if current := r.sessionCountByUser[userID]; current <= 1 {
		delete(r.sessionCountByUser, userID)
	} else {
		r.sessionCountByUser[userID] = current - 1
	}
}

func (r *gatewaySessionRegistry) decrementAnonymousLiveLocked(codespaceUUID string) {
	if current := r.anonymousLive[codespaceUUID]; current <= 1 {
		delete(r.anonymousLive, codespaceUUID)
	} else {
		r.anonymousLive[codespaceUUID] = current - 1
	}
}

func (r *gatewaySessionRegistry) decrementLiveLocked(codespaceUUID string) {
	current := r.live[codespaceUUID]
	if current <= 1 {
		delete(r.live, codespaceUUID)
		return
	}
	r.live[codespaceUUID] = current - 1
}

func (r *gatewaySessionRegistry) deleteGatewaySessionLocked(id string) {
	session := r.sessions[id]
	if session == nil {
		return
	}
	delete(r.sessions, id)
	r.decrementSessionCountLocked(session.userID, session.codespaceUUID)
}

func (r *gatewaySessionRegistry) dropExpiredSessionsLocked(now time.Time) []context.CancelFunc {
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

func (r *gatewaySessionRegistry) sessionExpiredLocked(session *gatewaySession, now time.Time) bool {
	if session == nil {
		return true
	}
	if !session.established && now.Sub(session.created) > gatewaySessionConnectTimeout {
		return true
	}
	if r.ttl > 0 && now.Sub(session.created) > r.ttl {
		return true
	}
	return session.established && r.idleTimeout > 0 && now.Sub(session.lastActive) > r.idleTimeout
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
