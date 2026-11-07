// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package gateway proxies authenticated browser and SSH traffic to Codespace agents.
package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
)

var errGatewayAccessLimitReached = errors.New("gateway access limit reached")

const (
	defaultGatewaySessionRevalidateInterval = 5 * time.Minute
	defaultGatewayAccessAllowedTTL          = 30 * time.Second
	defaultGatewayAccessCacheMaxKeys        = 65536
)

type accessConfig struct {
	AllowedTTL                      time.Duration
	StreamRevalidateInterval        time.Duration
	MaxInflightTotal                int
	MaxInflightPerSession           int
	PublicMaxConnectionsPerEndpoint int
	PublicMaxConnectionsPerIP       int
	ValidationMaxInflight           int
}

type accessController struct {
	Config accessConfig
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
	controller *accessController
	key        gatewayPublicEndpointKey
	ip         string
	once       sync.Once
}

type gatewayRequestReservation struct {
	controller *accessController
	sessionID  string
	once       sync.Once
}

type gatewayAccessCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxKeys int
	allowed map[gatewayAuthorizationKey]time.Time
}

type gatewayAuthorizationKind string

const (
	gatewayAuthorizationKindEndpoint gatewayAuthorizationKind = "endpoint"
	gatewayAuthorizationKindPublic   gatewayAuthorizationKind = "public"
)

type gatewayAuthorizationKey struct {
	kind          gatewayAuthorizationKind
	siteUID       string
	userID        int64
	codespaceUUID string
	endpointID    string
}

type gatewayPublicEndpointKey struct {
	siteUID       string
	codespaceUUID string
	endpointID    string
}

func newAccessController(config accessConfig) *accessController {
	if config.AllowedTTL <= 0 {
		config.AllowedTTL = defaultGatewayAccessAllowedTTL
	}
	if config.StreamRevalidateInterval <= 0 {
		config.StreamRevalidateInterval = defaultGatewaySessionRevalidateInterval
	}
	return &accessController{
		Config:          config,
		cache:           newGatewayAccessCache(config.AllowedTTL),
		sessionInflight: make(map[string]int),
		publicEndpoint:  make(map[gatewayPublicEndpointKey]int),
		publicIP:        make(map[string]int),
		validationCalls: make(map[gatewayAuthorizationKey]*gatewayValidationCall),
	}
}

func newAccessControllerFromConfig(config configpkg.GatewayConfig) *accessController {
	return newAccessController(accessConfig{
		AllowedTTL:                      defaultGatewayAccessAllowedTTL,
		StreamRevalidateInterval:        config.Sessions.RevalidateInterval.ToStdlib(),
		MaxInflightTotal:                config.Limits.MaxInflightTotal,
		MaxInflightPerSession:           config.Limits.MaxInflightPerSession,
		PublicMaxConnectionsPerEndpoint: config.Limits.PublicMaxConnectionsPerEndpoint,
		PublicMaxConnectionsPerIP:       config.Limits.PublicMaxConnectionsPerIP,
		ValidationMaxInflight:           config.Limits.ValidationMaxInflight,
	})
}

func (c *accessController) reservePublic(siteUID, codespaceUUID, endpointID, ip string) (*gatewayPublicReservation, int) {
	if c == nil {
		return nil, http.StatusServiceUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.totalInflight >= c.Config.MaxInflightTotal {
		return nil, http.StatusServiceUnavailable
	}
	key := gatewayPublicEndpointKey{siteUID: siteUID, codespaceUUID: codespaceUUID, endpointID: endpointID}
	if c.publicEndpoint[key] >= c.Config.PublicMaxConnectionsPerEndpoint {
		return nil, http.StatusTooManyRequests
	}
	if c.publicIP[ip] >= c.Config.PublicMaxConnectionsPerIP {
		return nil, http.StatusTooManyRequests
	}
	c.totalInflight++
	c.publicEndpoint[key]++
	c.publicIP[ip]++
	return &gatewayPublicReservation{controller: c, key: key, ip: ip}, 0
}

func (c *accessController) ReserveRequest() (*gatewayRequestReservation, int) {
	if c == nil {
		return nil, http.StatusServiceUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.totalInflight >= c.Config.MaxInflightTotal {
		return nil, http.StatusServiceUnavailable
	}
	c.totalInflight++
	return &gatewayRequestReservation{controller: c}, 0
}

func (c *accessController) reserveSessionRequest(sessionID string) (*gatewayRequestReservation, int) {
	if c == nil {
		return nil, http.StatusServiceUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.totalInflight >= c.Config.MaxInflightTotal {
		return nil, http.StatusServiceUnavailable
	}
	if c.sessionInflight[sessionID] >= c.Config.MaxInflightPerSession {
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

func (c *accessController) ValidatePublicEndpoint(
	ctx context.Context,
	siteUID string,
	codespaceUUID string,
	endpointID string,
	now time.Time,
	validate func(context.Context) (gatewayAccessDecision, error),
) (gatewayAccessDecision, bool, error) {
	key := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindPublic,
		siteUID:       siteUID,
		codespaceUUID: codespaceUUID,
		endpointID:    endpointID,
	}
	return c.validateAccess(ctx, key, now, validate)
}

func (c *accessController) validateEndpointSession(
	ctx context.Context,
	siteUID string,
	userID int64,
	codespaceUUID string,
	endpointID string,
	now time.Time,
	validate func(context.Context) (gatewayAccessDecision, error),
) (gatewayAccessDecision, bool, error) {
	key := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindEndpoint,
		siteUID:       siteUID,
		userID:        userID,
		codespaceUUID: codespaceUUID,
		endpointID:    endpointID,
	}
	return c.validateAccess(ctx, key, now, validate)
}

func (c *accessController) revalidatePublicEndpoint(
	ctx context.Context,
	siteUID string,
	codespaceUUID string,
	endpointID string,
	validate func(context.Context) (gatewayAccessDecision, error),
) (gatewayAccessDecision, bool, error) {
	key := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindPublic,
		siteUID:       siteUID,
		codespaceUUID: codespaceUUID,
		endpointID:    endpointID,
	}
	return c.validateAccessUncached(ctx, key, validate)
}

func (c *accessController) RevalidateEndpointSession(
	ctx context.Context,
	siteUID string,
	userID int64,
	codespaceUUID string,
	endpointID string,
	validate func(context.Context) (gatewayAccessDecision, error),
) (gatewayAccessDecision, bool, error) {
	key := gatewayAuthorizationKey{
		kind:          gatewayAuthorizationKindEndpoint,
		siteUID:       siteUID,
		userID:        userID,
		codespaceUUID: codespaceUUID,
		endpointID:    endpointID,
	}
	return c.validateAccessUncached(ctx, key, validate)
}

func (c *accessController) validateAccess(
	ctx context.Context,
	key gatewayAuthorizationKey,
	now time.Time,
	validate func(context.Context) (gatewayAccessDecision, error),
) (gatewayAccessDecision, bool, error) {
	if c.cache.IsAllowed(key, now) {
		return gatewayAccessDecision{Allowed: true}, false, nil
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
	if err == nil && decision.Allowed {
		c.cache.MarkAllowed(key, time.Now())
	}
	c.finishValidation(key, call, decision, err)
	return decision, false, err
}

func (c *accessController) validateAccessUncached(
	ctx context.Context,
	key gatewayAuthorizationKey,
	validate func(context.Context) (gatewayAccessDecision, error),
) (gatewayAccessDecision, bool, error) {
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
	c.finishValidation(key, call, decision, err)
	return decision, false, err
}

func (c *accessController) beginValidation(key gatewayAuthorizationKey) (*gatewayValidationCall, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if call := c.validationCalls[key]; call != nil {
		return call, false, true
	}
	if c.validationInflight >= c.Config.ValidationMaxInflight {
		return nil, false, false
	}
	call := &gatewayValidationCall{done: make(chan struct{})}
	c.validationCalls[key] = call
	c.validationInflight++
	return call, true, true
}

func (c *accessController) finishValidation(
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
	if ttl <= 0 {
		ttl = time.Second
	}
	return &gatewayAccessCache{
		ttl:     ttl,
		maxKeys: defaultGatewayAccessCacheMaxKeys,
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

	if _, exists := c.allowed[key]; !exists && c.maxKeys > 0 && len(c.allowed) >= c.maxKeys {
		c.pruneExpiredLocked(now)
		if len(c.allowed) >= c.maxKeys {
			c.pruneOldestLocked()
		}
	}
	c.allowed[key] = now.Add(c.ttl)
}

func (c *gatewayAccessCache) pruneExpiredLocked(now time.Time) {
	for key, expires := range c.allowed {
		if expires.IsZero() || !now.Before(expires) {
			delete(c.allowed, key)
		}
	}
}

func (c *gatewayAccessCache) pruneOldestLocked() {
	var oldestKey gatewayAuthorizationKey
	var oldest time.Time
	for key, expires := range c.allowed {
		if oldest.IsZero() || expires.Before(oldest) {
			oldestKey = key
			oldest = expires
		}
	}
	if !oldest.IsZero() {
		delete(c.allowed, oldestKey)
	}
}

func decrementGatewayCounter[K comparable](values map[K]int, key K) {
	current := values[key]
	if current <= 1 {
		delete(values, key)
		return
	}
	values[key] = current - 1
}
