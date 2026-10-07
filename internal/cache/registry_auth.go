// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

const AuthenticationTTL = 30 * time.Second
const cacheAuthenticationCapacity = 1024

func (c *registryCache) verifyToken(ctx context.Context, value string) (Session, error) {
	if len(value) < 26 || len(value) > 512 || len(c.tokenKey) < 32 {
		return Session{}, errors.New("cache registry session is invalid")
	}
	hash := sha256.Sum256([]byte(value))
	key := hex.EncodeToString(hash[:])
	now := time.Now()
	c.sessionsMu.Lock()
	cached, found := c.sessions[key]
	c.sessionsMu.Unlock()
	if found && now.Before(cached.expires) {
		return cached.Session, nil
	}
	session, err := VerifyCredential(c.tokenKey, c.id, value, now)
	if err != nil {
		return Session{}, errors.New("cache registry session is invalid")
	}
	if session.Namespace == "" || session.CacheID != c.id {
		return Session{}, errors.New("cache registry session is invalid")
	}
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	if c.sessions == nil {
		c.sessions = make(map[string]cachedCacheSession)
	}
	for key, entry := range c.sessions {
		if !now.Before(entry.expires) {
			delete(c.sessions, key)
		}
	}
	if len(c.sessions) >= cacheAuthenticationCapacity {
		for key := range c.sessions {
			delete(c.sessions, key)
			break
		}
	}
	expires := now.Add(AuthenticationTTL)
	if session.ExpiresAt.Before(expires) {
		expires = session.ExpiresAt
	}
	c.sessions[key] = cachedCacheSession{Session: session, expires: expires}
	return session, nil
}
