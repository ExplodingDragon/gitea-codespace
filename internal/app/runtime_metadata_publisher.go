// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

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
	wake chan struct{}
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
	p.mu.Lock()
	worker, ok := p.workers[codespaceUUID]
	if !ok && p.ctx != nil {
		worker = &runtimeMetadataWorker{wake: make(chan struct{}, 1)}
		p.workers[codespaceUUID] = worker
		go p.runWorker(p.ctx, codespaceUUID, worker)
		ok = true
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	worker.notify()
}

func (p *runtimeMetadataPublisher) PublishRuntimeMetadata(ctx context.Context, codespaceUUID string) error {
	if p == nil || p.state == nil || p.controlPlane == nil {
		return fmt.Errorf("runtime metadata publisher is not ready")
	}
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
			if !waitRuntimeMetadataRetry(ctx, p.retryInterval) {
				return fmt.Errorf("report runtime metadata %s generation %d: %w", codespaceUUID, generation, err)
			}
			continue
		}
		p.NotifyRuntimeMetadata(codespaceUUID)
		return nil
	}
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
