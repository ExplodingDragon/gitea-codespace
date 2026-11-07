// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package healthcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStateHandler(t *testing.T) {
	state := &State{}
	assertStatus := func(path string, expected int) {
		t.Helper()
		response := httptest.NewRecorder()
		state.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, expected, response.Code)
	}

	assertStatus("/healthz", http.StatusOK)
	assertStatus("/readyz", http.StatusServiceUnavailable)

	state.Ready()
	assertStatus("/healthz", http.StatusOK)
	assertStatus("/readyz", http.StatusOK)

	state.NotReady()
	assertStatus("/healthz", http.StatusOK)
	assertStatus("/readyz", http.StatusServiceUnavailable)

	state.Fail()
	state.Ready()
	assertStatus("/healthz", http.StatusServiceUnavailable)
	assertStatus("/readyz", http.StatusServiceUnavailable)
}

func TestRunStopsWorkloadBeforeReturning(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, "127.0.0.1:0", &State{}, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil
		})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("health supervisor did not start its workload")
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("health supervisor did not stop")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("health supervisor returned before its workload stopped")
	}
}
