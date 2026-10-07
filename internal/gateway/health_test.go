// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayReadinessDistinguishesControlPlaneWarning(t *testing.T) {
	health := NewProcessHealth()
	response := httptest.NewRecorder()
	health.writeReadyz(response)
	require.Equal(t, http.StatusOK, response.Code)

	health.Warn()
	response = httptest.NewRecorder()
	health.writeHealthz(response)
	require.Equal(t, http.StatusOK, response.Code)
	response = httptest.NewRecorder()
	health.writeReadyz(response)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)

	health.Recover()
	response = httptest.NewRecorder()
	health.writeReadyz(response)
	require.Equal(t, http.StatusOK, response.Code)
}
