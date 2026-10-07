// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"net/http"
	"sync/atomic"
	"time"
)

const SessionCookieName = "gitea_codespace_session"

const (
	HTTPMaxHeaderBytes = 64 * 1024
	HTTPReadHeaderTime = 10 * time.Second
)

type healthStatus int32

const (
	healthStatusPass healthStatus = iota
	healthStatusWarn
	healthStatusFail
)

type ProcessHealth struct {
	status atomic.Int32
}

func NewProcessHealth() *ProcessHealth {
	health := &ProcessHealth{}
	health.status.Store(int32(healthStatusPass))
	return health
}

func (h *ProcessHealth) Warn() {
	h.status.CompareAndSwap(int32(healthStatusPass), int32(healthStatusWarn))
}

func (h *ProcessHealth) Recover() {
	h.status.Store(int32(healthStatusPass))
}

func (h *ProcessHealth) Fail() {
	h.status.Store(int32(healthStatusFail))
}

func (h *ProcessHealth) writeHealthz(writer http.ResponseWriter) {
	switch healthStatus(h.status.Load()) {
	case healthStatusWarn:
		writeJSON(writer, http.StatusOK, map[string]any{"status": "warn"})
	case healthStatusFail:
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"status": "fail"})
	default:
		writeJSON(writer, http.StatusOK, map[string]any{"status": "pass"})
	}
}

func (h *ProcessHealth) writeReadyz(writer http.ResponseWriter) {
	if healthStatus(h.status.Load()) != healthStatusPass {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"status": "not ready"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ready"})
}
