// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errGatewayAccessLimitReached = errors.New("gateway access limit reached")

type gatewayAccessConfig struct {
	allowedTTL                      time.Duration
	maxInflightTotal                int
	maxInflightPerSession           int
	publicMaxConnectionsPerEndpoint int
	publicMaxConnectionsPerIP       int
	validationMaxInflight           int
}

type gatewayAccessController struct {
	config gatewayAccessConfig
	cache  *gatewayAccessCache

	mu                 sync.Mutex
	totalInflight      int
	validationInflight int
	sessionInflight    map[string]int
	publicEndpoint     map[gatewayPublicEndpointKey]int
	publicIP           map[string]int
	validationCalls    map[gatewayAuthorizationKey]*gatewayValidationCall
}

type gatewayValidationCall struct {
	done     chan struct{}
	decision gatewayAccessDecision
	err      error
}

type gatewayPublicReservation struct {
	controller *gatewayAccessController
	key        gatewayPublicEndpointKey
	ip         string
	once       sync.Once
}

type gatewayRequestReservation struct {
	controller *gatewayAccessController
	sessionID  string
	once       sync.Once
}

type gatewayAccessCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	allowed map[gatewayAuthorizationKey]time.Time
}

type gatewayAuthorizationKind string

const (
	gatewayAuthorizationKindEndpoint gatewayAuthorizationKind = "endpoint"
	gatewayAuthorizationKindPublic   gatewayAuthorizationKind = "public"
)

type gatewayAuthorizationKey struct {
	kind          gatewayAuthorizationKind
	userID        int64
	codespaceUUID string
	endpointID    string
}

type gatewayPublicEndpointKey struct {
	codespaceUUID string
	endpointID    string
}

func newGatewayAccessController(config gatewayAccessConfig) *gatewayAccessController {
	if config.allowedTTL <= 0 {
		config.allowedTTL = time.Second
	}
	return &gatewayAccessController{
		config:          config,
		cache:           newGatewayAccessCache(config.allowedTTL),
		sessionInflight: make(map[string]int),
		publicEndpoint:  make(map[gatewayPublicEndpointKey]int),
		publicIP:        make(map[string]int),
		validationCalls: make(map[gatewayAuthorizationKey]*gatewayValidationCall),
	}
}

func newGatewayAccessControllerFromConfig(config GatewayConfig) *gatewayAccessController {
	return newGatewayAccessController(gatewayAccessConfig{
		allowedTTL:                      time.Second,
		maxInflightTotal:                config.MaxInflightTotal,
		maxInflightPerSession:           config.MaxInflightPerSession,
		publicMaxConnectionsPerEndpoint: config.PublicMaxConnectionsPerEndpoint,
		publicMaxConnectionsPerIP:       config.PublicMaxConnectionsPerIP,
		validationMaxInflight:           config.ValidationMaxInflight,
	})
}

func (c *gatewayAccessController) reservePublic(codespaceUUID, endpointID, ip string) (*gatewayPublicReservation, int) {
	if c == nil {
		return nil, http.StatusServiceUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.totalInflight >= c.config.maxInflightTotal {
		return nil, http.StatusServiceUnavailable
	}
	key := gatewayPublicEndpointKey{codespaceUUID: codespaceUUID, endpointID: endpointID}
	if c.publicEndpoint[key] >= c.config.publicMaxConnectionsPerEndpoint {
		return nil, http.StatusTooManyRequests
	}
	if c.publicIP[ip] >= c.config.publicMaxConnectionsPerIP {
		return nil, http.StatusTooManyRequests
	}
	c.totalInflight++
	c.publicEndpoint[key]++
	c.publicIP[ip]++
	return &gatewayPublicReservation{controller: c, key: key, ip: ip}, 0
}

func (c *gatewayAccessController) reserveRequest() (*gatewayRequestReservation, int) {
	if c == nil {
		return nil, http.StatusServiceUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.totalInflight >= c.config.maxInflightTotal {
		return nil, http.StatusServiceUnavailable
	}
	c.totalInflight++
	return &gatewayRequestReservation{controller: c}, 0
}

func (c *gatewayAccessController) reserveSessionRequest(sessionID string) (*gatewayRequestReservation, int) {
	if c == nil {
		return nil, http.StatusServiceUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.totalInflight >= c.config.maxInflightTotal {
		return nil, http.StatusServiceUnavailable
	}
	if c.sessionInflight[sessionID] >= c.config.maxInflightPerSession {
		return nil, http.StatusTooManyRequests
	}
	c.totalInflight++
	c.sessionInflight[sessionID]++
	return &gatewayRequestReservation{controller: c, sessionID: sessionID}, 0
}

func (r *gatewayPublicReservation) Release() {
	if r == nil || r.controller == nil {
		return
	}
	r.once.Do(func() {
		c := r.controller
		c.mu.Lock()
		defer c.mu.Unlock()

		c.totalInflight--
		decrementGatewayCounter(c.publicEndpoint, r.key)
		decrementGatewayCounter(c.publicIP, r.ip)
	})
}

func (r *gatewayRequestReservation) Release() {
	if r == nil || r.controller == nil {
		return
	}
	r.once.Do(func() {
		c := r.controller
		c.mu.Lock()
		defer c.mu.Unlock()

		c.totalInflight--
		if r.sessionID != "" {
			decrementGatewayCounter(c.sessionInflight, r.sessionID)
		}
	})
}

func (c *gatewayAccessController) validatePublicEndpoint(
	ctx context.Context,
	codespaceUUID string,
	endpointID string,
	now time.Time,
	validate func(context.Context) (gatewayAccessDecision, error),
) (gatewayAccessDecision, bool, error) {
	key := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindPublic,
		codespaceUUID: codespaceUUID,
		endpointID:    endpointID,
	}
	return c.validateAccess(ctx, key, now, validate)
}

func (c *gatewayAccessController) validateEndpointSession(
	ctx context.Context,
	userID int64,
	codespaceUUID string,
	endpointID string,
	now time.Time,
	validate func(context.Context) (gatewayAccessDecision, error),
) (gatewayAccessDecision, bool, error) {
	key := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindEndpoint,
		userID:        userID,
		codespaceUUID: codespaceUUID,
		endpointID:    endpointID,
	}
	return c.validateAccess(ctx, key, now, validate)
}

func (c *gatewayAccessController) validateAccess(
	ctx context.Context,
	key gatewayAuthorizationKey,
	now time.Time,
	validate func(context.Context) (gatewayAccessDecision, error),
) (gatewayAccessDecision, bool, error) {
	if c.cache.IsAllowed(key, now) {
		return gatewayAccessDecision{allowed: true}, false, nil
	}
	call, leader, ok := c.beginValidation(key)
	if !ok {
		return gatewayAccessDecision{}, true, errGatewayAccessLimitReached
	}
	if !leader {
		select {
		case <-call.done:
			return call.decision, false, call.err
		case <-ctx.Done():
			return gatewayAccessDecision{}, false, ctx.Err()
		}
	}

	decision, err := validate(ctx)
	if err == nil && decision.allowed {
		c.cache.MarkAllowed(key, time.Now())
	}
	c.finishValidation(key, call, decision, err)
	return decision, false, err
}

func (c *gatewayAccessController) beginValidation(key gatewayAuthorizationKey) (*gatewayValidationCall, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if call := c.validationCalls[key]; call != nil {
		return call, false, true
	}
	if c.validationInflight >= c.config.validationMaxInflight {
		return nil, false, false
	}
	call := &gatewayValidationCall{done: make(chan struct{})}
	c.validationCalls[key] = call
	c.validationInflight++
	return call, true, true
}

func (c *gatewayAccessController) finishValidation(
	key gatewayAuthorizationKey,
	call *gatewayValidationCall,
	decision gatewayAccessDecision,
	err error,
) {
	c.mu.Lock()
	defer c.mu.Unlock()

	call.decision = decision
	call.err = err
	delete(c.validationCalls, key)
	c.validationInflight--
	close(call.done)
}

func newGatewayAccessCache(ttl time.Duration) *gatewayAccessCache {
	return &gatewayAccessCache{
		ttl:     ttl,
		allowed: make(map[gatewayAuthorizationKey]time.Time),
	}
}

func (c *gatewayAccessCache) IsAllowed(key gatewayAuthorizationKey, now time.Time) bool {
	if c == nil || key.codespaceUUID == "" || key.endpointID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	expires := c.allowed[key]
	if expires.IsZero() || !now.Before(expires) {
		delete(c.allowed, key)
		return false
	}
	return true
}

func (c *gatewayAccessCache) MarkAllowed(key gatewayAuthorizationKey, now time.Time) {
	if c == nil || key.codespaceUUID == "" || key.endpointID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.allowed[key] = now.Add(c.ttl)
}

func decrementGatewayCounter[K comparable](values map[K]int, key K) {
	current := values[key]
	if current <= 1 {
		delete(values, key)
		return
	}
	values[key] = current - 1
}
