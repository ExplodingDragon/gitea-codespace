// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"path/filepath"
	"testing"
	"time"
)

func newTestCodespaceStateStore(t *testing.T, dir string) *CodespaceStateStore {
	t.Helper()
	block, err := aes.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	store, err := startEmbeddedInfrastructureStore(filepath.Join(dir, "etcd"), defaultManagerStateEtcdPrefix, managerSecretCodec{aad: []byte("test-state"), gcm: gcm})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	leader, err := store.waitForLeadership(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = leader.close(ctx)
	})
	return NewCodespaceStateStore(&executionStateStore{store: store, leader: leader, siteID: 1})
}

func readTestCheckpoint(store *CodespaceStateStore, key string) ([]byte, error) {
	content, _, err := store.records.load(key)
	return content, err
}
