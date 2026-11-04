// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"testing"
	"time"
)

func TestGatewaySSHAuthLimiterWindow(t *testing.T) {
	limiter := newGatewaySSHAuthLimiter(gatewaySSHAuthLimitConfig{
		maxPerIP: 2, maxPerCodespace: 2, maxPerIPCodespace: 2, maxPerPublicKey: 2,
		backoffBase: time.Second, backoffMax: 4 * time.Second, failureWindow: 10 * time.Minute, maxKeys: 32,
	})
	now := time.Unix(1_700_000_000, 0)

	limiter.RecordFailure("192.0.2.1", "runtime", "key", "codespace_not_running", now)
	if !limiter.Allow("192.0.2.1", "runtime", "key", now) {
		t.Fatal("one failure exhausted a limit of two")
	}
	limiter.RecordFailure("192.0.2.1", "runtime", "key", "codespace_not_running", now)
	if limiter.Allow("192.0.2.1", "runtime", "key", now) {
		t.Fatal("the configured failure limit was not enforced")
	}
	if !limiter.Allow("192.0.2.1", "runtime", "key", now.Add(time.Minute)) {
		t.Fatal("the one-minute rate-limit window did not reset")
	}
}

func TestGatewaySSHAuthLimiterBackoff(t *testing.T) {
	limiter := newGatewaySSHAuthLimiter(gatewaySSHAuthLimitConfig{
		maxPerIP: 10, maxPerCodespace: 10, maxPerIPCodespace: 10, maxPerPublicKey: 10,
		backoffBase: time.Second, backoffMax: 4 * time.Second, failureWindow: 10 * time.Minute, maxKeys: 32,
	})
	now := time.Unix(1_700_000_000, 0)

	limiter.RecordFailure("192.0.2.1", "runtime", "key", "invalid_credentials", now)
	if limiter.Allow("192.0.2.1", "runtime", "key", now.Add(500*time.Millisecond)) {
		t.Fatal("credential failure did not apply the initial backoff")
	}
	if !limiter.Allow("192.0.2.1", "runtime", "key", now.Add(time.Second)) {
		t.Fatal("the initial backoff did not expire")
	}

	second := now.Add(time.Second)
	limiter.RecordFailure("192.0.2.1", "runtime", "key", "invalid_credentials", second)
	if limiter.Allow("192.0.2.1", "runtime", "key", second.Add(time.Second)) {
		t.Fatal("a repeated credential failure did not increase the backoff")
	}
	if !limiter.Allow("192.0.2.1", "runtime", "key", second.Add(2*time.Second)) {
		t.Fatal("the increased backoff did not expire")
	}
}
