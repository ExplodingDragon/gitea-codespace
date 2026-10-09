// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"fmt"
	"math"
	"sync"
	"time"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/google/uuid"
)

// Permit measures execution authority from request send time, including reconnects.
type Permit struct {
	mu       sync.Mutex
	session  string
	sequence uint64
	sent     time.Time
	version  int64
	deadline time.Time
}

func (p *Permit) Reconnect() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = uuid.NewString()
	p.sequence, p.version = 0, 0
	p.sent, p.deadline = time.Time{}, time.Time{}
	return p.session
}

func (p *Permit) Request(version int64, now time.Time) (string, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == "" || version < 0 || p.sequence == math.MaxUint64 {
		return "", 0, fmt.Errorf("control session or sequence is invalid")
	}
	if version != p.version {
		p.deadline = time.Time{}
	}
	p.sequence++
	p.sent, p.version = now, version
	return p.session, p.sequence, nil
}

func (p *Permit) Accept(response *agentv1.ControlResponse, now time.Time) (time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if response == nil || response.SessionId != p.session || response.Sequence != p.sequence || p.sequence == 0 {
		return time.Time{}, fmt.Errorf("control reply does not match the current request")
	}
	if response.CancelExecution {
		p.deadline = time.Time{}
		return time.Time{}, fmt.Errorf("execution canceled by Manager")
	}
	if response.OperationRversion <= 0 || (p.version != 0 && response.OperationRversion != p.version) {
		return time.Time{}, fmt.Errorf("control reply does not match the current operation")
	}
	duration := response.PermitValidForMilliseconds
	if duration <= 0 || duration > math.MaxInt64/int64(time.Millisecond) {
		return time.Time{}, fmt.Errorf("execution permit duration is invalid")
	}
	deadline := p.sent.Add(time.Duration(duration) * time.Millisecond)
	if !now.Before(deadline) {
		return time.Time{}, fmt.Errorf("execution permit expired before its reply arrived")
	}
	p.version, p.deadline = response.OperationRversion, deadline
	return deadline, nil
}

func (p *Permit) Valid(version int64, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return version > 0 && version == p.version && now.Before(p.deadline)
}
