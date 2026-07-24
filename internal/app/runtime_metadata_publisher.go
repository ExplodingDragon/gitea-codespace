// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"connectrpc.com/connect"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
)

const defaultRuntimeMetadataRetryInterval = time.Second

type runtimeMetadataNotifier interface {
	NotifyRuntimeMetadata(codespaceUUID string)
}

type runtimeMetadataPublisher struct {
	state         *CodespaceStateStore
	controlPlane  *gatewayControlPlane
	retryInterval time.Duration

	mu      sync.Mutex
	ctx     context.Context
	workers map[string]*runtimeMetadataWorker

	refreshInterval time.Duration
	refreshWake     chan struct{}
	refreshStarted  bool
}

type runtimeMetadataWorker struct {
	wake      chan struct{}
	publishMu sync.Mutex
}

func newRuntimeMetadataPublisher(state *CodespaceStateStore, controlPlane *gatewayControlPlane, retryInterval time.Duration) *runtimeMetadataPublisher {
	if retryInterval <= 0 {
		retryInterval = defaultRuntimeMetadataRetryInterval
	}
	return &runtimeMetadataPublisher{
		state:         state,
		controlPlane:  controlPlane,
		retryInterval: retryInterval,
		workers:       make(map[string]*runtimeMetadataWorker),
		refreshWake:   make(chan struct{}, 1),
	}
}

func (p *runtimeMetadataPublisher) SaveManagerServiceSettings(settings manager.ManagerServiceSettings) error {
	if settings.RuntimeMetadataRefreshInterval <= 0 {
		return fmt.Errorf("runtime metadata refresh interval must be positive")
	}
	p.mu.Lock()
	p.refreshInterval = settings.RuntimeMetadataRefreshInterval
	p.mu.Unlock()
	p.wakeRefresh()
	return nil
}

func (p *runtimeMetadataPublisher) NotifyRuntimeMetadata(codespaceUUID string) {
	if p == nil || p.state == nil || p.controlPlane == nil {
		return
	}
	worker, ok := p.ensureWorker(codespaceUUID)
	if !ok {
		return
	}
	worker.notify()
}

func (p *runtimeMetadataPublisher) PublishRuntimeMetadata(ctx context.Context, codespaceUUID string) error {
	if p == nil || p.state == nil || p.controlPlane == nil {
		return fmt.Errorf("runtime metadata publisher is not ready")
	}
	worker, _ := p.ensureWorker(codespaceUUID)
	worker.publishMu.Lock()
	defer worker.publishMu.Unlock()

	for {
		generation, metadataJSON, ok, err := p.state.LoadRuntimeMetadataRequest(codespaceUUID)
		if err != nil {
			if !waitRuntimeMetadataRetry(ctx, p.retryInterval) {
				return fmt.Errorf("load runtime metadata %s: %w", codespaceUUID, err)
			}
			continue
		}
		if !ok {
			return fmt.Errorf("runtime metadata snapshot %s is missing", codespaceUUID)
		}
		if err := p.controlPlane.reportRuntimeMetadata(ctx, codespaceUUID, metadataJSON, generation); err != nil {
			if handled, handleErr := p.handleMetadataPublishError(codespaceUUID, err); handled {
				if handleErr != nil {
					return handleErr
				}
				continue
			}
			if !waitRuntimeMetadataRetry(ctx, p.retryInterval) {
				return fmt.Errorf("report runtime metadata %s generation %d: %w", codespaceUUID, generation, err)
			}
			continue
		}
		p.NotifyRuntimeMetadata(codespaceUUID)
		return nil
	}
}

func (p *runtimeMetadataPublisher) ensureWorker(codespaceUUID string) (*runtimeMetadataWorker, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	worker, ok := p.workers[codespaceUUID]
	if !ok {
		worker = &runtimeMetadataWorker{wake: make(chan struct{}, 1)}
		p.workers[codespaceUUID] = worker
		if p.ctx != nil {
			go p.runWorker(p.ctx, codespaceUUID, worker)
			ok = true
		}
	} else if p.ctx != nil {
		ok = true
	}
	return worker, ok
}

func (p *runtimeMetadataPublisher) Run(ctx context.Context, codespaceUUIDs []string) {
	if p == nil || p.state == nil || p.controlPlane == nil {
		return
	}
	p.mu.Lock()
	p.ctx = ctx
	if !p.refreshStarted {
		p.refreshStarted = true
		go p.runRefresh(ctx)
	}
	for _, codespaceUUID := range codespaceUUIDs {
		if _, ok := p.workers[codespaceUUID]; ok {
			continue
		}
		worker := &runtimeMetadataWorker{wake: make(chan struct{}, 1)}
		p.workers[codespaceUUID] = worker
		go p.runWorker(ctx, codespaceUUID, worker)
		worker.notify()
	}
	p.mu.Unlock()
}

func (p *runtimeMetadataPublisher) runRefresh(ctx context.Context) {
	for {
		interval := p.currentRefreshInterval()
		if interval <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-p.refreshWake:
				continue
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-p.refreshWake:
			stopRuntimeMetadataTimer(timer)
			continue
		case <-timer.C:
		}

		codespaceUUIDs, err := p.state.LoadRuntimeMetadataCodespaceUUIDs()
		if err != nil {
			log.Printf("load runtime metadata refresh set: %v", err)
			continue
		}
		for _, codespaceUUID := range codespaceUUIDs {
			p.NotifyRuntimeMetadata(codespaceUUID)
		}
	}
}

func (p *runtimeMetadataPublisher) currentRefreshInterval() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.refreshInterval
}

func (p *runtimeMetadataPublisher) runWorker(ctx context.Context, codespaceUUID string, worker *runtimeMetadataWorker) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-worker.wake:
			p.publishUntilCurrent(ctx, codespaceUUID, worker)
		}
	}
}

func (p *runtimeMetadataPublisher) publishUntilCurrent(ctx context.Context, codespaceUUID string, worker *runtimeMetadataWorker) {
	worker.publishMu.Lock()
	defer worker.publishMu.Unlock()

	for {
		generation, metadataJSON, ok, err := p.state.LoadRuntimeMetadataRequest(codespaceUUID)
		if err != nil {
			log.Printf("load runtime metadata %s: %v", codespaceUUID, err)
			if !waitRuntimeMetadataRetry(ctx, p.retryInterval) {
				return
			}
			continue
		}
		if !ok {
			return
		}
		if err := p.controlPlane.reportRuntimeMetadata(ctx, codespaceUUID, metadataJSON, generation); err != nil {
			if handled, handleErr := p.handleMetadataPublishError(codespaceUUID, err); handled {
				if handleErr != nil {
					log.Printf("report runtime metadata %s generation %d: %v", codespaceUUID, generation, handleErr)
					return
				}
				continue
			}
			log.Printf("report runtime metadata %s generation %d: %v", codespaceUUID, generation, err)
			if !waitRuntimeMetadataRetry(ctx, p.retryInterval) {
				return
			}
			continue
		}
		if !worker.consumePendingWake() {
			return
		}
	}
}

func (p *runtimeMetadataPublisher) handleMetadataPublishError(codespaceUUID string, err error) (bool, error) {
	category, staleGeneration, ok := metadataFailure(err)
	if !ok {
		return false, nil
	}
	switch category {
	case "stale_generation":
		if err := p.state.RebaseRuntimeMetadataGeneration(codespaceUUID, staleGeneration); err != nil {
			return true, fmt.Errorf("rebase runtime metadata %s from stale generation %d: %w", codespaceUUID, staleGeneration, err)
		}
		return true, nil
	case "generation_conflict", "version_exhausted":
		return true, fmt.Errorf("report runtime metadata %s: %s: %w", codespaceUUID, category, err)
	default:
		return false, nil
	}
}

func metadataFailure(err error) (string, int64, bool) {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return "", 0, false
	}
	var category string
	var staleGeneration int64
	for _, detail := range connectErr.Details() {
		value, valueErr := detail.Value()
		if valueErr != nil {
			continue
		}
		switch typed := value.(type) {
		case *codespacev1.FailureDetail:
			category = typed.GetCategory()
		case *codespacev1.StaleGenerationDetail:
			staleGeneration = typed.GetCurrentGeneration()
		}
	}
	return category, staleGeneration, category != ""
}

func (w *runtimeMetadataWorker) notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (p *runtimeMetadataPublisher) wakeRefresh() {
	select {
	case p.refreshWake <- struct{}{}:
	default:
	}
}

func (w *runtimeMetadataWorker) consumePendingWake() bool {
	select {
	case <-w.wake:
		return true
	default:
		return false
	}
}

func waitRuntimeMetadataRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func stopRuntimeMetadataTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}
