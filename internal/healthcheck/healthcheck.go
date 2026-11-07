// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package healthcheck serves the private Kubernetes health endpoints used by
// managed Codespace components.
package healthcheck

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// AddressEnvironment overrides the private health listener address.
	AddressEnvironment = "GITEA_CODESPACE_HEALTH_ADDRESS"
	// DefaultAddress listens on every Pod interface so kubelet can probe it.
	DefaultAddress = ":8081"
	// DefaultPort is the container port corresponding to DefaultAddress.
	DefaultPort = int32(8081)
)

const (
	statusStarting int32 = iota
	statusReady
	statusUnhealthy
)

// State tracks whether a process is live and able to receive business traffic.
// Its zero value is live but not ready.
type State struct {
	status atomic.Int32
}

// Ready marks the process ready unless it has entered the terminal unhealthy state.
func (s *State) Ready() {
	s.transition(statusReady)
}

// NotReady marks a live process unable to receive business traffic.
func (s *State) NotReady() {
	s.transition(statusStarting)
}

// Fail marks the process unhealthy. This terminal state cannot be recovered.
func (s *State) Fail() {
	s.status.Store(statusUnhealthy)
}

func (s *State) transition(next int32) {
	for {
		current := s.status.Load()
		if current == statusUnhealthy || current == next {
			return
		}
		if s.status.CompareAndSwap(current, next) {
			return
		}
	}
}

// Address returns the configured health listener address.
func Address() string {
	if address := strings.TrimSpace(os.Getenv(AddressEnvironment)); address != "" {
		return address
	}
	return DefaultAddress
}

// Handler returns the health-only HTTP handler.
func (s *State) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		if s.status.Load() == statusUnhealthy {
			writeStatus(writer, http.StatusServiceUnavailable, "unhealthy\n")
			return
		}
		writeStatus(writer, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if s.status.Load() != statusReady {
			writeStatus(writer, http.StatusServiceUnavailable, "not ready\n")
			return
		}
		writeStatus(writer, http.StatusOK, "ok\n")
	})
	return mux
}

func writeStatus(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}

// Run serves health checks while supervising the component workload.
func Run(ctx context.Context, address string, state *State, workload func(context.Context) error) error {
	if state == nil {
		return errors.New("health state is required")
	}
	if workload == nil {
		return errors.New("health workload is required")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	healthCtx, stopHealth := context.WithCancel(context.Background())
	defer stopHealth()
	server := &http.Server{Handler: state.Handler(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8 * 1024}
	server.BaseContext = func(_ net.Listener) context.Context { return healthCtx }
	healthDone := make(chan error, 1)
	go func() {
		healthDone <- server.Serve(listener)
	}()

	workCtx, stopWork := context.WithCancel(ctx)
	defer stopWork()
	workDone := make(chan error, 1)
	go func() {
		workDone <- workload(workCtx)
	}()

	var workErr error
	select {
	case workErr = <-workDone:
	case serveErr := <-healthDone:
		state.Fail()
		stopWork()
		workErr = <-workDone
		if serveErr == nil {
			serveErr = errors.New("health server stopped unexpectedly")
		}
		return errors.Join(serveErr, workErr)
	case <-ctx.Done():
		state.NotReady()
		stopWork()
		workErr = <-workDone
	}

	state.NotReady()
	stopWork()
	stopHealth()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdown)
	serveErr := <-healthDone
	if errors.Is(serveErr, http.ErrServerClosed) || errors.Is(serveErr, net.ErrClosed) {
		serveErr = nil
	}
	return errors.Join(workErr, shutdownErr, serveErr)
}
