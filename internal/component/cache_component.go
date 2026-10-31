// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package component

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	cachepkg "gitea.dev/codespace/internal/cache"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"gitea.dev/codespace/internal/rpc/component/v1/componentv1connect"
	"github.com/google/uuid"
)

const componentProtocolVersion uint32 = 1

type cacheControlStream interface {
	Send(*componentv1.CacheControlRequest) error
	Receive() (*componentv1.CacheControlResponse, error)
	Close() error
}

// CacheComponentClient multiplexes configuration, ownership heartbeats, and
// cleanup grants over one long-lived Manager control stream.
type CacheComponentClient struct {
	Client          componentv1connect.ComponentServiceClient `json:"-"`
	SecretDirectory string                                    `json:"-"`

	mu           sync.Mutex
	sendMu       sync.Mutex
	cancelStream context.CancelFunc
	stream       cacheControlStream
	cacheID      string
	sessionID    string
	sequence     uint64
	config       cachepkg.ControlConfig
	permitUntil  time.Time
	streamErr    error
	ready        chan struct{}
	waiters      map[uint64]chan *componentv1.CacheControlResponse
	sent         map[uint64]time.Time
}

func (c *CacheComponentClient) failLocked(err error) {
	if c.stream != nil && c.streamErr == nil {
		if c.cancelStream != nil {
			c.cancelStream()
		} else {
			_ = c.stream.Close()
		}
	}
	c.streamErr = err
	clear(c.sent)
	for sequence, waiter := range c.waiters {
		close(waiter)
		delete(c.waiters, sequence)
	}
	select {
	case <-c.ready:
	default:
		close(c.ready)
	}
}

func (c *CacheComponentClient) start(ctx context.Context, id string) error {
	c.mu.Lock()
	if c.stream != nil && c.streamErr == nil {
		defer c.mu.Unlock()
		if c.cacheID != id {
			return fmt.Errorf("cache control client is already bound to %q", c.cacheID)
		}
		return nil
	}
	directory := c.SecretDirectory
	if directory == "" {
		directory = "/var/run/codespace/cache"
	}
	registryKey, err := os.ReadFile(filepath.Join(directory, "registry-key"))
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("read cache registry key: %w", err)
	}
	tokenKey, err := os.ReadFile(filepath.Join(directory, "token-key"))
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("read cache token key: %w", err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	timeout := time.AfterFunc(ComponentRequestTimeout, cancel)
	defer timeout.Stop()
	stream, err := c.Client.CacheControl(streamCtx)
	if err != nil {
		cancel()
		c.mu.Unlock()
		return err
	}
	c.stream, c.cacheID, c.sessionID = stream, id, uuid.NewString()
	c.cancelStream = cancel
	c.sequence, c.config, c.streamErr = 1, cachepkg.ControlConfig{RegistryKey: string(registryKey), TokenKey: append([]byte(nil), tokenKey...)}, nil
	c.ready = make(chan struct{})
	c.waiters = make(map[uint64]chan *componentv1.CacheControlResponse)
	c.sent = map[uint64]time.Time{1: time.Now()}
	if err := stream.Send(&componentv1.CacheControlRequest{ProtocolVersion: componentProtocolVersion, CacheId: id, SessionId: c.sessionID, Sequence: c.sequence}); err != nil {
		c.failLocked(err)
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	go c.receive(ctx, stream, directory)
	return nil
}

func (c *CacheComponentClient) receive(ctx context.Context, stream cacheControlStream, directory string) {
	for {
		response, err := stream.Receive()
		c.mu.Lock()
		if c.stream != stream {
			c.mu.Unlock()
			return
		}
		if err != nil {
			c.failLocked(err)
			c.mu.Unlock()
			return
		}
		if response.ProtocolVersion != componentProtocolVersion || response.PermitValidForMilliseconds < 0 || response.Sequence > c.sequence {
			c.failLocked(fmt.Errorf("manager returned an invalid cache control response"))
			c.mu.Unlock()
			return
		}
		if sent, ok := c.sent[response.Sequence]; ok && response.PermitValidForMilliseconds > 0 {
			delete(c.sent, response.Sequence)
			deadline := sent.Add(time.Duration(response.PermitValidForMilliseconds)*time.Millisecond - time.Second)
			if !time.Now().Before(deadline) {
				c.failLocked(fmt.Errorf("cache control acknowledgment arrived after its permit expired"))
				c.mu.Unlock()
				return
			}
			if deadline.After(c.permitUntil) {
				c.permitUntil = deadline
			}
		}
		if response.Config != nil {
			config, configErr := cacheConfigFromProto(response.Config)
			if configErr == nil && config.Storage.Driver == "s3" {
				accessKey, accessErr := os.ReadFile(filepath.Join(directory, "s3-access-key"))
				secretKey, secretErr := os.ReadFile(filepath.Join(directory, "s3-secret-key"))
				if accessErr != nil || secretErr != nil {
					configErr = fmt.Errorf("read cache S3 credentials")
				} else {
					config.Storage.S3.AccessKey, config.Storage.S3.SecretKey = string(accessKey), string(secretKey)
				}
			}
			if configErr == nil {
				configErr = config.Validate()
			}
			if configErr != nil {
				c.failLocked(configErr)
				c.mu.Unlock()
				return
			}
			c.config.Config = config
			select {
			case <-c.ready:
			default:
				close(c.ready)
			}
		}
		if waiter := c.waiters[response.Sequence]; waiter != nil && response.PermitValidForMilliseconds > 0 {
			delete(c.waiters, response.Sequence)
			waiter <- response
			close(waiter)
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			c.mu.Lock()
			if c.stream == stream {
				c.failLocked(ctx.Err())
			}
			c.mu.Unlock()
			return
		default:
		}
	}
}

func (c *CacheComponentClient) GetCache(ctx context.Context, id string) (cachepkg.ControlConfig, error) {
	if err := c.start(ctx, id); err != nil {
		return cachepkg.ControlConfig{}, fmt.Errorf("start cache control: %w", err)
	}
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	timer := time.NewTimer(ComponentRequestTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return cachepkg.ControlConfig{}, ctx.Err()
	case <-timer.C:
		c.mu.Lock()
		if c.ready == ready {
			c.failLocked(context.DeadlineExceeded)
		}
		c.mu.Unlock()
		return cachepkg.ControlConfig{}, fmt.Errorf("wait for cache configuration: %w", context.DeadlineExceeded)
	case <-ready:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streamErr != nil {
		return cachepkg.ControlConfig{}, fmt.Errorf("cache control: %w", c.streamErr)
	}
	if !time.Now().Before(c.permitUntil) {
		c.failLocked(fmt.Errorf("cache control permit expired"))
		return cachepkg.ControlConfig{}, fmt.Errorf("cache control permit expired")
	}
	return c.config, nil
}

func (c *CacheComponentClient) ClaimCache(ctx context.Context, id string) (cachepkg.Owner, error) {
	config, err := c.GetCache(ctx, id)
	if err != nil {
		return cachepkg.Owner{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cachepkg.Owner{CacheID: id, ID: c.sessionID, ConfigRevision: config.Config.Revision, ExpiresAt: c.permitUntil}, nil
}

func (c *CacheComponentClient) send(ctx context.Context, status cachepkg.Status, request *componentv1.CacheControlRequest) (*componentv1.CacheControlResponse, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.stream == nil || c.streamErr != nil || !time.Now().Before(c.permitUntil) {
		c.mu.Unlock()
		return nil, fmt.Errorf("cache control permit expired")
	}
	c.sequence++
	request.ProtocolVersion, request.CacheId, request.SessionId, request.Sequence = componentProtocolVersion, c.cacheID, c.sessionID, c.sequence
	request.Status = cacheStatusToProto(status)
	waiter := make(chan *componentv1.CacheControlResponse, 1)
	c.waiters[c.sequence] = waiter
	if c.sent == nil {
		c.sent = make(map[uint64]time.Time)
	}
	c.sent[c.sequence] = time.Now()
	stream, cancelStream, deadline := c.stream, c.cancelStream, c.permitUntil
	c.mu.Unlock()
	if timeout := time.Now().Add(ComponentRequestTimeout); timeout.Before(deadline) {
		deadline = timeout
	}
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	stopSend := context.AfterFunc(waitCtx, func() {
		if cancelStream != nil {
			cancelStream()
		} else {
			_ = stream.Close()
		}
	})
	defer stopSend()
	err := stream.Send(request)
	stopSend()
	c.mu.Lock()
	if err != nil {
		c.failLocked(err)
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.waiters[request.Sequence] == waiter {
			delete(c.waiters, request.Sequence)
		}
		delete(c.sent, request.Sequence)
	}()
	select {
	case <-waitCtx.Done():
		if request.Maintenance == nil {
			c.mu.Lock()
			if c.stream == stream {
				c.failLocked(waitCtx.Err())
			}
			c.mu.Unlock()
		}
		return nil, waitCtx.Err()
	case response, ok := <-waiter:
		if !ok {
			return nil, fmt.Errorf("cache control closed before the request was acknowledged")
		}
		return response, nil
	}
}

func (c *CacheComponentClient) HeartbeatCache(ctx context.Context, _ cachepkg.Owner, status cachepkg.Status) (time.Time, error) {
	_, err := c.send(ctx, status, &componentv1.CacheControlRequest{})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.permitUntil, err
}

func (c *CacheComponentClient) ReleaseCache(context.Context, cachepkg.Owner) error {
	c.mu.Lock()
	if c.stream == nil {
		c.mu.Unlock()
		return nil
	}
	stream := c.stream
	if c.cancelStream != nil {
		c.cancelStream()
	}
	c.stream = nil
	c.failLocked(context.Canceled)
	c.mu.Unlock()
	return stream.Close()
}

func (c *CacheComponentClient) BeginCacheGC(ctx context.Context, owner cachepkg.Owner) (cachepkg.GCGrant, error) {
	response, err := c.send(ctx, cachepkg.Status{ConfigRevision: owner.ConfigRevision}, &componentv1.CacheControlRequest{Maintenance: &componentv1.CacheControlRequest_BeginGc{BeginGc: &componentv1.CacheGCRequest{}}})
	if err != nil {
		return cachepkg.GCGrant{}, err
	}
	grant := response.GcGrant
	if grant == nil || grant.CacheId == "" || grant.Id == "" {
		return cachepkg.GCGrant{}, fmt.Errorf("cache GC grant is incomplete")
	}
	return cachepkg.GCGrant{CacheID: grant.CacheId, ID: grant.Id, ConfigRevision: grant.ConfigRevision, ExpiresAt: time.UnixMilli(grant.ExpiresAtUnixMilliseconds)}, nil
}

func (c *CacheComponentClient) CompleteCacheGC(ctx context.Context, grant cachepkg.GCGrant) error {
	_, err := c.send(ctx, cachepkg.Status{ConfigRevision: grant.ConfigRevision}, &componentv1.CacheControlRequest{Maintenance: &componentv1.CacheControlRequest_CompleteGc{CompleteGc: &componentv1.CacheGCComplete{GrantId: grant.ID}}})
	return err
}

var _ cachepkg.Control = (*CacheComponentClient)(nil)
