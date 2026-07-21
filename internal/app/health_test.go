// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProcessHealthzPass(t *testing.T) {
	t.Parallel()

	health := newProcessHealth()
	response := requestHealthz(t, newGatewayHandler(health, newGatewaySessionRegistry(), newTestGatewayAccess(), nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status code = %d", response.Code)
	}
	assertHealthzBody(t, response.Body.Bytes(), "pass")
}

func TestProcessHealthzWarn(t *testing.T) {
	t.Parallel()

	health := newProcessHealth()
	health.Warn()
	response := requestHealthz(t, newGatewayHandler(health, newGatewaySessionRegistry(), newTestGatewayAccess(), nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status code = %d", response.Code)
	}
	assertHealthzBody(t, response.Body.Bytes(), "warn")
}

func TestProcessHealthzFail(t *testing.T) {
	t.Parallel()

	health := newProcessHealth()
	health.Fail()
	response := requestHealthz(t, newGatewayHandler(health, newGatewaySessionRegistry(), newTestGatewayAccess(), nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d", response.Code)
	}
	assertHealthzBody(t, response.Body.Bytes(), "fail")
}

func TestRuntimeAPIUsesProcessHealth(t *testing.T) {
	t.Parallel()

	health := newProcessHealth()
	health.Fail()
	response := requestHealthz(t, newRuntimeAPIHandler(health))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d", response.Code)
	}
	assertHealthzBody(t, response.Body.Bytes(), "fail")
}

func requestHealthz(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertHealthzBody(t *testing.T, body []byte, status string) {
	t.Helper()

	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode healthz response: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["status"] != status {
		t.Fatalf("status = %q", payload["status"])
	}
}
