// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const maxGatewaySSHAuthLimitKeys = 65536

type gatewaySSHAuthLimitConfig struct {
	maxPerIP          int
	maxPerCodespace   int
	maxPerIPCodespace int
	maxPerPublicKey   int
	backoffBase       time.Duration
	backoffMax        time.Duration
	failureWindow     time.Duration
	maxKeys           int
}

type gatewaySSHAuthLimiter struct {
	config gatewaySSHAuthLimitConfig

	mu      sync.Mutex
	records map[gatewaySSHAuthLimitKey]*gatewaySSHAuthLimitRecord
}

type gatewaySSHAuthLimitKey struct {
	kind  string
	value string
}

type gatewaySSHAuthLimitRecord struct {
	windowStart  time.Time
	windowCount  int
	failures     int
	lastSeen     time.Time
	backoffUntil time.Time
}

func newGatewaySSHAuthLimiterFromConfig(config GatewayConfig) *gatewaySSHAuthLimiter {
	return newGatewaySSHAuthLimiter(gatewaySSHAuthLimitConfig{
		maxPerIP:          config.SSHAuthMaxAttemptsPerIP,
		maxPerCodespace:   config.SSHAuthMaxAttemptsPerCodespace,
		maxPerIPCodespace: config.SSHAuthMaxAttemptsPerIPCodespace,
		maxPerPublicKey:   config.SSHAuthMaxAttemptsPerPublicKey,
		backoffBase:       config.SSHAuthBackoffBase.ToStdlib(),
		backoffMax:        config.SSHAuthBackoffMax.ToStdlib(),
		failureWindow:     config.SSHAuthFailureWindow.ToStdlib(),
		maxKeys:           maxGatewaySSHAuthLimitKeys,
	})
}

func newGatewaySSHAuthLimiter(config gatewaySSHAuthLimitConfig) *gatewaySSHAuthLimiter {
	if config.maxPerIP <= 0 {
		config.maxPerIP = DefaultConfig().Gateway.SSHAuthMaxAttemptsPerIP
	}
	if config.maxPerCodespace <= 0 {
		config.maxPerCodespace = DefaultConfig().Gateway.SSHAuthMaxAttemptsPerCodespace
	}
	if config.maxPerIPCodespace <= 0 {
		config.maxPerIPCodespace = DefaultConfig().Gateway.SSHAuthMaxAttemptsPerIPCodespace
	}
	if config.maxPerPublicKey <= 0 {
		config.maxPerPublicKey = DefaultConfig().Gateway.SSHAuthMaxAttemptsPerPublicKey
	}
	if config.backoffBase <= 0 {
		config.backoffBase = DefaultConfig().Gateway.SSHAuthBackoffBase.ToStdlib()
	}
	if config.backoffMax <= 0 {
		config.backoffMax = DefaultConfig().Gateway.SSHAuthBackoffMax.ToStdlib()
	}
	if config.failureWindow <= 0 {
		config.failureWindow = DefaultConfig().Gateway.SSHAuthFailureWindow.ToStdlib()
	}
	if config.maxKeys <= 0 {
		config.maxKeys = maxGatewaySSHAuthLimitKeys
	}
	return &gatewaySSHAuthLimiter{
		config:  config,
		records: make(map[gatewaySSHAuthLimitKey]*gatewaySSHAuthLimitRecord),
	}
}

func (l *gatewaySSHAuthLimiter) Allow(sourceIP, codespaceUUID, publicKeyHash string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneExpired(now)
	for _, item := range l.items(sourceIP, codespaceUUID, publicKeyHash) {
		record := l.records[item.key]
		if record == nil {
			if len(l.records) >= l.config.maxKeys {
				return false
			}
			continue
		}
		if now.Before(record.backoffUntil) {
			return false
		}
		if record.windowStart.IsZero() || now.Sub(record.windowStart) >= time.Minute {
			continue
		}
		if record.windowCount >= item.limit {
			return false
		}
	}
	return true
}

func (l *gatewaySSHAuthLimiter) RecordFailure(sourceIP, codespaceUUID, publicKeyHash, category string, now time.Time) {
	if l == nil {
		return
	}
	if !gatewaySSHAuthFailureCounts(category) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneExpired(now)
	for _, item := range l.items(sourceIP, codespaceUUID, publicKeyHash) {
		record := l.records[item.key]
		if record == nil {
			if len(l.records) >= l.config.maxKeys {
				continue
			}
			record = &gatewaySSHAuthLimitRecord{}
			l.records[item.key] = record
		}
		if record.windowStart.IsZero() || now.Sub(record.windowStart) >= time.Minute {
			record.windowStart = now
			record.windowCount = 0
		}
		record.windowCount++
		record.failures++
		record.lastSeen = now
		if gatewaySSHAuthFailureBacksOff(category) {
			record.backoffUntil = now.Add(l.backoffDuration(record.failures))
		}
	}
}

func (l *gatewaySSHAuthLimiter) pruneExpired(now time.Time) {
	for key, record := range l.records {
		if now.Sub(record.lastSeen) >= l.config.failureWindow {
			delete(l.records, key)
		}
	}
}

func (l *gatewaySSHAuthLimiter) backoffDuration(failures int) time.Duration {
	backoff := l.config.backoffBase
	for i := 1; i < failures && backoff < l.config.backoffMax; i++ {
		backoff *= 2
	}
	if backoff > l.config.backoffMax {
		return l.config.backoffMax
	}
	return backoff
}

func (l *gatewaySSHAuthLimiter) items(sourceIP, codespaceUUID, publicKeyHash string) []gatewaySSHAuthLimitItem {
	items := make([]gatewaySSHAuthLimitItem, 0, 4)
	if sourceIP != "" {
		items = append(items, gatewaySSHAuthLimitItem{
			key:   gatewaySSHAuthLimitKey{kind: "source_ip", value: sourceIP},
			limit: l.config.maxPerIP,
		})
	}
	if codespaceUUID != "" {
		items = append(items, gatewaySSHAuthLimitItem{
			key:   gatewaySSHAuthLimitKey{kind: "codespace_uuid", value: codespaceUUID},
			limit: l.config.maxPerCodespace,
		})
	}
	if sourceIP != "" && codespaceUUID != "" {
		items = append(items, gatewaySSHAuthLimitItem{
			key:   gatewaySSHAuthLimitKey{kind: "source_ip_codespace_uuid", value: sourceIP + "\x00" + codespaceUUID},
			limit: l.config.maxPerIPCodespace,
		})
	}
	if publicKeyHash != "" {
		items = append(items, gatewaySSHAuthLimitItem{
			key:   gatewaySSHAuthLimitKey{kind: "public_key_hash", value: publicKeyHash},
			limit: l.config.maxPerPublicKey,
		})
	}
	return items
}

type gatewaySSHAuthLimitItem struct {
	key   gatewaySSHAuthLimitKey
	limit int
}

func gatewaySSHAuthFailureCounts(category string) bool {
	switch strings.TrimSpace(category) {
	case "invalid_credentials", "codespace_not_found", "codespace_not_running", "login_restricted", "manager_mismatch":
		return true
	default:
		return false
	}
}

func gatewaySSHAuthFailureBacksOff(category string) bool {
	switch strings.TrimSpace(category) {
	case "invalid_credentials", "codespace_not_found":
		return true
	default:
		return false
	}
}

func gatewaySSHPublicKeyHash(key ssh.PublicKey) string {
	if key == nil {
		return ""
	}
	sum := sha256.Sum256(key.Marshal())
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

func gatewaySSHSourceIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	if tcpAddr, ok := addr.(*net.TCPAddr); ok {
		return tcpAddr.IP.String()
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err == nil {
		return host
	}
	return addr.String()
}

func gatewaySSHAuthLimitError() error {
	return fmt.Errorf("ssh authentication rate limit reached")
}
