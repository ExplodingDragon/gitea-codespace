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
	CloseRequest() error
	CloseResponse() error
}

// CacheComponentClient multiplexes configuration, ownership heartbeats, and
// cleanup grants over one long-lived Manager control stream.
type CacheComponentClient struct {
	Client          componentv1connect.ComponentServiceClient `json:"-"`
	SecretDirectory string                                    `json:"-"`

	mu          sync.Mutex
	stream      cacheControlStream
	cacheID     string
	sessionID   string
	sequence    uint64
	config      cachepkg.ControlConfig
	permitUntil time.Time
	streamErr   error
	ready       chan struct{}
	waiters     map[uint64]chan *componentv1.CacheControlResponse
}

func (c *CacheComponentClient) failLocked(err error) {
	c.streamErr = err
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
	stream := c.Client.CacheControl(ctx)
	c.stream, c.cacheID, c.sessionID = stream, id, uuid.NewString()
	c.sequence, c.config, c.streamErr = 1, cachepkg.ControlConfig{RegistryKey: string(registryKey), TokenKey: append([]byte(nil), tokenKey...)}, nil
	c.ready = make(chan struct{})
	c.waiters = make(map[uint64]chan *componentv1.CacheControlResponse)
	if err := stream.Send(&componentv1.CacheControlRequest{ProtocolVersion: componentProtocolVersion, CacheId: id, SessionId: c.sessionID, Sequence: c.sequence}); err != nil {
		c.streamErr = err
		close(c.ready)
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
		if response.ProtocolVersion != componentProtocolVersion || response.PermitValidForMilliseconds <= 0 {
			c.failLocked(fmt.Errorf("manager returned an invalid cache control response"))
			c.mu.Unlock()
			return
		}
		c.permitUntil = time.Now().Add(time.Duration(response.PermitValidForMilliseconds) * time.Millisecond)
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
		if waiter := c.waiters[response.Sequence]; waiter != nil {
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
	select {
	case <-ctx.Done():
		return cachepkg.ControlConfig{}, ctx.Err()
	case <-ready:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streamErr != nil {
		return cachepkg.ControlConfig{}, fmt.Errorf("cache control: %w", c.streamErr)
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

func (c *CacheComponentClient) send(status cachepkg.Status, request *componentv1.CacheControlRequest) (<-chan *componentv1.CacheControlResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream == nil || c.streamErr != nil || !time.Now().Before(c.permitUntil) {
		return nil, fmt.Errorf("cache control permit expired")
	}
	c.sequence++
	request.ProtocolVersion, request.CacheId, request.SessionId, request.Sequence = componentProtocolVersion, c.cacheID, c.sessionID, c.sequence
	request.Status = cacheStatusToProto(status)
	var waiter chan *componentv1.CacheControlResponse
	if request.Maintenance != nil {
		waiter = make(chan *componentv1.CacheControlResponse, 1)
		c.waiters[c.sequence] = waiter
	}
	if err := c.stream.Send(request); err != nil {
		delete(c.waiters, c.sequence)
		c.streamErr = err
		return nil, err
	}
	return waiter, nil
}

func (c *CacheComponentClient) HeartbeatCache(_ context.Context, _ cachepkg.Owner, status cachepkg.Status) error {
	_, err := c.send(status, &componentv1.CacheControlRequest{})
	return err
}

func (c *CacheComponentClient) ReleaseCache(context.Context, cachepkg.Owner) error {
	c.mu.Lock()
	if c.stream == nil {
		c.mu.Unlock()
		return nil
	}
	stream := c.stream
	c.stream = nil
	c.failLocked(context.Canceled)
	c.mu.Unlock()
	err := stream.CloseRequest()
	_ = stream.CloseResponse()
	return err
}

func (c *CacheComponentClient) BeginCacheGC(ctx context.Context, owner cachepkg.Owner) (cachepkg.GCGrant, error) {
	waiter, err := c.send(cachepkg.Status{ConfigRevision: owner.ConfigRevision}, &componentv1.CacheControlRequest{Maintenance: &componentv1.CacheControlRequest_BeginGc{BeginGc: &componentv1.CacheGCRequest{}}})
	if err != nil {
		return cachepkg.GCGrant{}, err
	}
	select {
	case <-ctx.Done():
		return cachepkg.GCGrant{}, ctx.Err()
	case response, ok := <-waiter:
		if !ok || response == nil || response.GcGrant == nil {
			return cachepkg.GCGrant{}, fmt.Errorf("manager did not grant cache cleanup")
		}
		return cacheGCGrantFromProto(response.GcGrant)
	}
}

func (c *CacheComponentClient) CompleteCacheGC(_ context.Context, grant cachepkg.GCGrant) error {
	_, err := c.send(cachepkg.Status{ConfigRevision: grant.ConfigRevision}, &componentv1.CacheControlRequest{Maintenance: &componentv1.CacheControlRequest_CompleteGc{CompleteGc: &componentv1.CacheGCComplete{GrantId: grant.ID}}})
	return err
}

var _ cachepkg.Control = (*CacheComponentClient)(nil)
