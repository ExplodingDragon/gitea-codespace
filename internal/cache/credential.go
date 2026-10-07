// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
)

type credentialClaims struct {
	CacheID   string `json:"c"`
	Namespace string `json:"n"`
	Policy    uint8  `json:"p"`
	Expires   int64  `json:"e"`
}

// SignCredential creates a cache-scoped bearer credential. Each cache has a
// distinct key, so validation does not require a Manager round trip.
func SignCredential(secret []byte, cacheID, namespace string, policy configpkg.CachePolicy, expires time.Time) (string, error) {
	claims := credentialClaims{CacheID: cacheID, Namespace: namespace, Expires: expires.Unix()}
	if policy.Public {
		claims.Policy |= 1
	}
	if policy.Build {
		claims.Policy |= 2
	}
	if len(secret) < 32 || cacheID == "" || namespace == "" || claims.Policy == 0 || !expires.After(time.Now()) {
		return "", fmt.Errorf("invalid cache credential claims")
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyCredential verifies one locally supplied cache credential and returns
// its repository namespace and allowed registry operations.
func VerifyCredential(secret []byte, cacheID, token string, now time.Time) (Session, error) {
	payloadText, signatureText, ok := strings.Cut(token, ".")
	if !ok || len(secret) < 32 {
		return Session{}, fmt.Errorf("invalid cache credential")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadText)
	if err != nil {
		return Session{}, fmt.Errorf("invalid cache credential")
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureText)
	if err != nil {
		return Session{}, fmt.Errorf("invalid cache credential")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return Session{}, fmt.Errorf("invalid cache credential")
	}
	var claims credentialClaims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.CacheID != cacheID || claims.Namespace == "" || claims.Policy == 0 || claims.Expires <= now.Unix() {
		return Session{}, fmt.Errorf("expired or invalid cache credential")
	}
	return Session{CacheID: claims.CacheID, Namespace: claims.Namespace, Policy: configpkg.CachePolicy{Public: claims.Policy&1 != 0, Build: claims.Policy&2 != 0}, ExpiresAt: time.Unix(claims.Expires, 0)}, nil
}
