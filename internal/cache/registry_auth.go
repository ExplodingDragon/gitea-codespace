// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const authenticationTTL = 30 * time.Second
const cacheAuthenticationCapacity = 1024
const registryAuthenticationService = "gitea-codespace-cache"

type registryTokenResponse struct {
	Token       string    `json:"token"`
	AccessToken string    `json:"access_token"`
	ExpiresIn   int64     `json:"expires_in"`
	IssuedAt    time.Time `json:"issued_at"`
}

func (c *registryCache) verifyToken(value string) (Session, error) {
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
	expires := now.Add(authenticationTTL)
	if session.ExpiresAt.Before(expires) {
		expires = session.ExpiresAt
	}
	c.sessions[key] = cachedCacheSession{Session: session, expires: expires}
	return session, nil
}

func (c *registryCache) registrySession(request *http.Request) (Session, error) {
	if username, password, ok := request.BasicAuth(); ok {
		if username != Username {
			return Session{}, errors.New("cache registry credentials are invalid")
		}
		return c.verifyToken(password)
	}
	fields := strings.Fields(request.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return Session{}, errors.New("cache registry credentials are required")
	}
	return c.verifyToken(fields[1])
}

func registryAuthenticationChallenge(request *http.Request, repository, action string) (string, error) {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	realm := (&url.URL{Scheme: scheme, Host: host, Path: "/auth"}).String()
	parsed, err := url.Parse(realm)
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("cache registry request host is invalid")
	}
	challenge := fmt.Sprintf(`Bearer realm=%q,service=%q`, realm, registryAuthenticationService)
	if repository == "" {
		return challenge, nil
	}
	actions := action
	if action == "push" {
		actions = "pull,push"
	}
	return challenge + fmt.Sprintf(`,scope=%q`, "repository:"+repository+":"+actions), nil
}

func (c *registryCache) serveRegistryToken(writer http.ResponseWriter, request *http.Request) {
	var username, token string
	switch request.Method {
	case http.MethodGet:
		username, token, _ = request.BasicAuth()
	case http.MethodPost:
		request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
		if err := request.ParseForm(); err != nil {
			http.Error(writer, "invalid registry token request", http.StatusBadRequest)
			return
		}
		username, token = request.Form.Get("username"), request.Form.Get("password")
	default:
		writer.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(writer, "registry token method is not allowed", http.StatusMethodNotAllowed)
		return
	}
	if username != Username {
		http.Error(writer, "cache registry credentials are invalid", http.StatusUnauthorized)
		return
	}
	session, err := c.verifyToken(token)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusUnauthorized)
		return
	}
	now := time.Now().UTC()
	expiresIn := int64(time.Until(session.ExpiresAt).Seconds())
	if expiresIn < 1 {
		http.Error(writer, "cache registry session is invalid", http.StatusUnauthorized)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(registryTokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   expiresIn,
		IssuedAt:    now,
	}); err != nil {
		return
	}
}
