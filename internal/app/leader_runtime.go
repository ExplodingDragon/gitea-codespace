// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
)

func runLeaderLoop(ctx context.Context, output io.Writer, initial InfrastructureRuntimeConfig) error {
	for ctx.Err() == nil {
		// Normal shutdown keeps this lease alive until the execution term has drained.
		leaseCtx, stopLease := context.WithCancel(context.WithoutCancel(ctx))
		stopWaiting := context.AfterFunc(ctx, stopLease)
		leader, err := initial.store.waitForLeadership(leaseCtx, initial.NodeID)
		stopped := stopWaiting()
		if err != nil || !stopped || ctx.Err() != nil {
			stopLease()
			if leader != nil {
				closeCtx, cancelClose := context.WithTimeout(context.Background(), initial.Config.Node.ShutdownTimeout.ToStdlib())
				_ = leader.close(closeCtx)
				cancelClose()
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		termCtx, cancelTerm := context.WithCancel(ctx)
		stopLost := context.AfterFunc(leader.session.Ctx(), cancelTerm)
		_, _ = fmt.Fprintf(output, "codespace leader %s acquired revision %d\n", initial.NodeID, leader.revision)
		config, err := initial.store.LoadRuntimeConfig(termCtx)
		if err == nil {
			config.Config.provisionerKind = initial.Config.provisionerKind
			config.Config.runtimeExecutable = initial.Config.runtimeExecutable
			config.store = initial.store
			finished := make(chan error, 1)
			go func() { finished <- runExecutionTerm(termCtx, output, config, leader) }()
			select {
			case err = <-finished:
			case <-termCtx.Done():
				shutdown, stop := context.WithTimeout(context.Background(), config.Config.Node.ShutdownTimeout.ToStdlib())
				select {
				case err = <-finished:
				case <-shutdown.Done():
					err = shutdown.Err()
				}
				stop()
			}
		}
		cancelTerm()
		stopLost()
		lost := leader.session.Ctx().Err() != nil
		if errors.Is(err, context.DeadlineExceeded) {
			// An unfinished executor cannot certify a handover. Stop keepalive and
			// let the lease expire instead of explicitly making the key available.
			leader.session.Orphan()
			stopLease()
			return err
		}
		closeCtx, cancelClose := context.WithTimeout(context.Background(), initial.Config.Node.ShutdownTimeout.ToStdlib())
		closeErr := leader.close(closeCtx)
		cancelClose()
		stopLease()
		if ctx.Err() != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if !lost && err != nil {
			return errors.Join(err, closeErr)
		}
		log.Printf("codespace leadership ended: %v", errors.Join(err, closeErr))
		if err := waitElectionRetry(ctx); err != nil {
			return nil
		}
	}
	return nil
}

func runExecutionTerm(ctx context.Context, output io.Writer, config InfrastructureRuntimeConfig, leader *deploymentLeadership) error {
	state, err := loadProcessState(config, leader)
	if err != nil {
		return err
	}
	if err := validateProcessRuntimeBindings(ctx, state, config.store); err != nil {
		return err
	}
	state.gatewaySSHHostKey, err = config.store.LoadGatewaySSHHostKey(ctx)
	if err != nil {
		return err
	}
	cache, err := newRegistryCache(config.Config, registryCacheSecret(config.Sites))
	if err != nil {
		return err
	}
	listener, err := cache.OpenListener()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var cacheServer *http.Server
	if listener != nil {
		defer listener.Close()
		cacheServer = newRegistryCacheHTTPServer(cache)
		defer cacheServer.Close()
	}
	// This view adds the leader comparison to every runtime binding/route mutation.
	writer := &etcdInfrastructureStore{client: config.store.client, prefix: config.store.prefix, secret: config.store.secret, leadership: leader}
	runtime, err := newProcessRuntime(ctx, config.Config, state, cache, writer, true)
	if err != nil {
		return err
	}
	defer runtime.gatewayRoutes.Close()
	errorsCh := make(chan error, len(runtime.sites)+1)
	cacheErrors := make(chan error, 1)
	var workers sync.WaitGroup
	if cacheServer != nil {
		workers.Add(2)
		go func() {
			defer workers.Done()
			serveHTTP(ctx, cacheErrors, "cache registry", cacheServer, listener)
		}()
		go func() {
			defer workers.Done()
			cache.RunGC(ctx)
		}()
		_, _ = fmt.Fprintf(output, "codespace cache registry listening on %s\n", listener.Addr())
	}
	for _, site := range runtime.sites {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := site.agent.Run(ctx)
			site.publisher.Close()
			runtime.capacityCoordinator.Forget(site.siteID)
			errorsCh <- err
		}()
	}
	remaining := len(runtime.sites)
	for remaining > 0 && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case cacheErr := <-cacheErrors:
			err = errors.Join(err, cacheErr)
			cancel()
		case siteErr := <-errorsCh:
			err = errors.Join(err, siteErr)
			remaining--
			if siteErr != nil {
				log.Printf("manager site stopped: %v", siteErr)
			}
			if remaining == 0 && ctx.Err() == nil {
				err = errors.Join(err, errors.New("all manager sites stopped"))
			}
		}
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), config.Config.Node.ShutdownTimeout.ToStdlib())
	defer stop()
	if cacheServer != nil {
		if closeErr := cacheServer.Shutdown(shutdown); closeErr != nil {
			err = errors.Join(err, closeErr, cacheServer.Close())
		}
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
		close(errorsCh)
		for siteErr := range errorsCh {
			err = errors.Join(err, siteErr)
		}
	case <-shutdown.Done():
		return fmt.Errorf("execution term did not stop before shutdown deadline: %w", shutdown.Err())
	}
	return err
}
