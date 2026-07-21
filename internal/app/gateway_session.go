// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

const gatewaySessionConnectTimeout = 30 * time.Second

type gatewaySessionRegistry struct {
	mu       sync.Mutex
	live     map[string]int
	sessions map[string]*gatewaySession
}

type gatewaySession struct {
	id            string
	userID        int64
	codespaceUUID string
	endpointID    string
	created       time.Time
	established   bool
}

func newGatewaySessionRegistry() *gatewaySessionRegistry {
	return &gatewaySessionRegistry{
		live:     make(map[string]int),
		sessions: make(map[string]*gatewaySession),
	}
}

func (r *gatewaySessionRegistry) Create(binding gatewayOpenTokenBinding, now time.Time) (string, error) {
	if binding.userID <= 0 || binding.codespaceUUID == "" || binding.endpointID == "" {
		return "", fmt.Errorf("gateway session binding is incomplete")
	}
	id, err := newGatewaySessionID()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sessions[id] = &gatewaySession{
		id:            id,
		userID:        binding.userID,
		codespaceUUID: binding.codespaceUUID,
		endpointID:    binding.endpointID,
		created:       now,
		established:   false,
	}
	return id, nil
}

func (r *gatewaySessionRegistry) Authenticate(id, codespaceUUID, endpointID string, now time.Time) (gatewaySession, bool) {
	if id == "" || codespaceUUID == "" || endpointID == "" {
		return gatewaySession{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	session := r.sessions[id]
	if session == nil {
		return gatewaySession{}, false
	}
	if session.codespaceUUID != codespaceUUID || session.endpointID != endpointID {
		return gatewaySession{}, false
	}
	if !session.established && now.Sub(session.created) > gatewaySessionConnectTimeout {
		delete(r.sessions, id)
		return gatewaySession{}, false
	}
	session.established = true
	return *session, true
}

func (r *gatewaySessionRegistry) Begin(codespaceUUID string) func() {
	if codespaceUUID == "" {
		return func() {}
	}
	r.mu.Lock()
	r.live[codespaceUUID]++
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()

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
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.live[codespaceUUID]
}

func newGatewaySessionID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate gateway session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
