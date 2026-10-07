// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
)

var ErrCacheServiceBusy = errors.New("another cache service is active")

const cacheStatusScanTimeout = 2 * time.Second

func waitCacheManagerRetry(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isCacheAuthorizationError(err error) bool {
	code := connect.CodeOf(err)
	return code == connect.CodeUnauthenticated || code == connect.CodePermissionDenied
}

func Run(ctx context.Context, output io.Writer, control Control, id string) error {
	for ctx.Err() == nil {
		controlConfig, err := control.GetCache(ctx, id)
		if err != nil {
			if isCacheAuthorizationError(err) {
				return fmt.Errorf("cache Manager authorization failed: %w", err)
			}
			if waitCacheManagerRetry(ctx) != nil {
				return nil
			}
			continue
		}
		config := controlConfig.Config
		if config.ID != id || controlConfig.RegistryKey == "" || len(controlConfig.TokenKey) < 32 {
			return fmt.Errorf("manager returned an invalid cache configuration")
		}
		if !config.Enabled {
			if waitCacheManagerRetry(ctx) != nil {
				return nil
			}
			continue
		}
		instanceCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- runCacheInstance(instanceCtx, output, control, controlConfig) }()
		ticker := time.NewTicker(2 * time.Second)
		watching := true
		var terminalErr error
		for watching {
			select {
			case <-ctx.Done():
				watching = false
			case err := <-done:
				done = nil
				watching = false
				if err != nil && !errors.Is(err, ErrCacheServiceBusy) && ctx.Err() == nil {
					if isCacheAuthorizationError(err) {
						terminalErr = fmt.Errorf("cache Manager authorization failed: %w", err)
					} else {
						log.Printf("cache service: %v", err)
					}
				}
			case <-ticker.C:
				updated, loadErr := control.GetCache(ctx, id)
				if loadErr != nil {
					if isCacheAuthorizationError(loadErr) {
						terminalErr = fmt.Errorf("cache Manager authorization failed: %w", loadErr)
						watching = false
					}
					// Keep serving during a transient control-plane outage. The
					// Manager lease still fences the process after its TTL.
					continue
				}
				if updated.Config.ID != id || updated.RegistryKey == "" || len(updated.TokenKey) < 32 {
					terminalErr = fmt.Errorf("manager returned an invalid cache configuration")
					watching = false
					continue
				}
				if updated.Config.Revision != config.Revision {
					watching = false
				} else if !updated.Config.Enabled {
					watching = false
				}
			}
		}
		ticker.Stop()
		cancel()
		if done != nil {
			<-done
		}
		if terminalErr != nil {
			return terminalErr
		}
	}
	return nil
}

func runCacheInstance(ctx context.Context, output io.Writer, control Control, controlConfig ControlConfig) (resultErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	config := controlConfig.Config
	owner, err := control.ClaimCache(ctx, config.ID)
	if err != nil {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, control.ReleaseCache(releaseCtx, owner))
	}()
	cache, err := New(ctx, config, controlConfig.RegistryKey, controlConfig.TokenKey)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cache.Close()) }()
	cache.owner = owner
	listener, err := cache.OpenListener()
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	server := &http.Server{
		Handler:           cache.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 * 1024,
	}
	server.BaseContext = func(_ net.Listener) context.Context { return ctx }
	defer func() { _ = server.Close() }()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); cache.maintain(ctx, control) }()
	_, _ = fmt.Fprintf(output, "codespace cache registry listening on %s\n", listener.Addr())
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	status := Status{PublicURL: cache.publicURL, ConfigRevision: config.Revision}
	var nextScan time.Time
	running := true
	for running {
		if !time.Now().Before(nextScan) {
			scanCtx, stopScan := context.WithTimeout(ctx, cacheStatusScanTimeout)
			cacheBytes, cacheErr := cacheBlobBytes(scanCtx, cache.driver)
			mirrorBytes := int64(0)
			for _, driver := range cache.mirrorDrivers {
				bytes, scanErr := cacheBlobBytes(scanCtx, driver)
				if scanErr != nil {
					cacheErr = errors.Join(cacheErr, scanErr)
					break
				}
				mirrorBytes += bytes
			}
			stopScan()
			if cacheErr == nil {
				status.CacheBytes = cacheBytes
				status.MirrorBytes = mirrorBytes
				status.ScannedAt = time.Now().UTC()
			}
			nextScan = time.Now().Add(5 * time.Minute)
		}
		cache.cleanupMu.Lock()
		status.LastCleanup, status.CleanupResult = cache.lastCleanup, cache.cleanupResult
		cache.cleanupMu.Unlock()
		if heartbeatErr := control.HeartbeatCache(ctx, owner, status); heartbeatErr != nil {
			resultErr = heartbeatErr
			// Stop maintenance before waiting for it below. A failed heartbeat
			// means Manager may have already assigned this cache to another
			// process, so the old process must stop all work immediately.
			cancel()
			break
		}
		select {
		case <-ctx.Done():
			running = false
		case err := <-serveDone:
			serveDone = nil
			resultErr = err
			cancel()
			running = false
		case <-ticker.C:
		}
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	if shutdownErr := server.Shutdown(shutdown); shutdownErr != nil {
		resultErr = errors.Join(resultErr, shutdownErr, server.Close())
	}
	if serveDone != nil {
		<-serveDone
	}
	<-maintenanceDone
	if errors.Is(resultErr, context.Canceled) || resultErr == http.ErrServerClosed {
		return nil
	}
	return resultErr
}

func (c *registryCache) maintain(ctx context.Context, control Control) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current, err := control.GetCache(ctx, c.id)
		if err != nil {
			continue
		}
		if !current.Config.Enabled || current.Config.Revision != c.configRevision {
			continue
		}
		if c.gcInterval <= 0 || time.Now().Before(c.lastCleanup.Add(c.gcInterval)) {
			continue
		}
		grant, err := control.BeginCacheGC(ctx, c.owner)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("cache cleanup authorization: %v", err)
			}
			continue
		}
		// Drain cached authentication before deleting data; stop before the grant expires.
		deadline := grant.ExpiresAt.Add(-5 * time.Second)
		if time.Until(deadline) <= AuthenticationTTL {
			_ = control.CompleteCacheGC(ctx, grant)
			continue
		}
		timer := time.NewTimer(AuthenticationTTL)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		gcCtx, stop := context.WithDeadline(ctx, deadline)
		err = c.prune(gcCtx)
		stop()
		completeErr := control.CompleteCacheGC(ctx, grant)
		if completeErr != nil {
			err = errors.Join(err, completeErr)
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("cache cleanup: %v", err)
		}
	}
}
