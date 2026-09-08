// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/etcd/server/v3/embed"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v3client"
)

func openEmbeddedInfrastructureStore() (_ *etcdInfrastructureStore, err error) {
	codec, err := newManagerSecretCodec()
	if err != nil {
		return nil, err
	}
	prefix, err := managerStateEtcdPrefix()
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(os.Getenv(managerStatePathEnv))
	if dir == "" {
		dir = filepath.Join("codespace-state", "etcd")
	}
	return startEmbeddedInfrastructureStore(dir, prefix, codec)
}

func startEmbeddedInfrastructureStore(dir, prefix string, codec managerSecretCodec) (_ *etcdInfrastructureStore, err error) {
	lock, err := acquireStateDirLock(dir)
	if err != nil {
		return nil, fmt.Errorf("lock embedded etcd directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect embedded etcd directory: %w", err)
	}
	cfg := embed.NewConfig()
	cfg.Name = "codespace"
	cfg.Dir = dir
	cfg.ListenPeerUrls = nil
	cfg.ListenClientUrls = nil
	cfg.ListenClientHttpUrls = nil
	cfg.ListenMetricsUrls = nil
	cfg.AdvertiseClientUrls = nil
	cfg.EnableGRPCGateway = false
	// A single member still needs bootstrap identity URLs, but no peer listener.
	cfg.AdvertisePeerUrls = []url.URL{{Scheme: "http", Host: "127.0.0.1:2380"}}
	cfg.InitialCluster = cfg.InitialClusterFromName(cfg.Name)
	cfg.AutoCompactionMode = "periodic"
	cfg.AutoCompactionRetention = "1h"
	cfg.LogLevel = "error"
	server, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil, fmt.Errorf("start embedded etcd: %w", err)
	}
	defer func() {
		if err != nil {
			server.Close()
		}
	}()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-server.Server.ReadyNotify():
	case err = <-server.Err():
		return nil, fmt.Errorf("embedded etcd failed before ready: %v", err)
	case <-timer.C:
		return nil, fmt.Errorf("embedded etcd startup timed out")
	}
	client := v3client.New(server.Server)
	// The directory lock and lack of listeners prove the previous local executor
	// is gone. Its persisted leader key can be removed before starting this process.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = client.Delete(ctx, prefix+"/leader"); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("clear previous embedded leader: %w", err)
	}
	return &etcdInfrastructureStore{client: client, prefix: prefix, secret: codec, embedded: server, stateLock: lock}, nil
}
