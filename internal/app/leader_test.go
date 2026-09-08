// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestDeploymentLeaderHandover(t *testing.T) {
	setInfrastructureStateEnv(t, t.TempDir())
	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first, err := store.waitForLeadership(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.close(ctx) }()
	state := &executionStateStore{store: store, leader: first, siteID: 1}
	if err := state.save("checkpoint", []byte("private startup input"), 0); err != nil {
		t.Fatal(err)
	}
	stored, err := store.client.Get(ctx, state.key("checkpoint"))
	if err != nil || len(stored.Kvs) != 1 {
		t.Fatalf("read checkpoint: %v", err)
	}
	if strings.Contains(string(stored.Kvs[0].Value), "private startup input") {
		t.Fatal("checkpoint was stored without encryption")
	}
	// A copied ciphertext cannot be read under a different site's identity.
	otherSite := &executionStateStore{store: store, leader: first, siteID: 2}
	if _, err := store.client.Put(ctx, otherSite.key("checkpoint"), string(stored.Kvs[0].Value)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := otherSite.load("checkpoint"); err == nil {
		t.Fatal("another site decrypted the checkpoint")
	}

	type electionResult struct {
		leader *deploymentLeadership
		err    error
	}
	next := make(chan electionResult, 1)
	go func() {
		leader, err := store.waitForLeadership(ctx, "node")
		next <- electionResult{leader, err}
	}()
	select {
	case result := <-next:
		t.Fatalf("candidate returned while leader is alive: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := first.close(ctx); err != nil {
		t.Fatal(err)
	}
	var second *deploymentLeadership
	select {
	case result := <-next:
		if result.err != nil {
			t.Fatal(result.err)
		}
		second = result.leader
	case <-ctx.Done():
		t.Fatal("candidate did not take over")
	}
	defer func() { _ = second.close(ctx) }()
	if second.revision <= first.revision || second.identity == first.identity {
		t.Fatal("successive terms share an identity")
	}
	recovered := &executionStateStore{store: store, leader: second, siteID: 1}
	content, revision, err := recovered.load("checkpoint")
	if err != nil || string(content) != "private startup input" {
		t.Fatalf("recover checkpoint: %q %v", content, err)
	}
	if err := state.save("checkpoint", []byte("stale"), revision); !errors.Is(err, errLeadershipLost) {
		t.Fatalf("old leader write: %v", err)
	}
	if err := recovered.save("checkpoint", []byte("new"), revision); err != nil {
		t.Fatal(err)
	}
	if err := recovered.save("checkpoint", []byte("stale revision"), revision); err == nil {
		t.Fatal("stale checkpoint revision was accepted")
	}
	_ = first.close(ctx)
	owner, err := store.client.Get(ctx, store.key("leader"))
	if err != nil || len(owner.Kvs) != 1 || string(owner.Kvs[0].Value) != second.identity {
		t.Fatalf("old leader shutdown changed owner: %v", err)
	}
	_, revision, err = recovered.load("checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.remove("checkpoint", revision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recovered.load("checkpoint"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted checkpoint: %v", err)
	}
}

func TestDeploymentLeaderLeaseRevoked(t *testing.T) {
	setInfrastructureStateEnv(t, t.TempDir())
	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := store.waitForLeadership(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.close(ctx) }()
	if _, err := store.client.Revoke(ctx, first.session.Lease()); err != nil {
		t.Fatal(err)
	}
	second, err := store.waitForLeadership(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.close(ctx) }()
	if err := first.commit(ctx, nil, clientv3.OpPut(store.key("test-result"), "stale")); !errors.Is(err, errLeadershipLost) {
		t.Fatalf("revoked leader commit: %v", err)
	}
}

func TestDeploymentLeaderConcurrentCandidates(t *testing.T) {
	setInfrastructureStateEnv(t, t.TempDir())
	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		leader *deploymentLeadership
		err    error
	}
	results := make(chan result, 3)
	start := make(chan struct{})
	for range 3 {
		go func() {
			<-start
			leader, err := store.waitForLeadership(ctx, "candidate")
			results <- result{leader, err}
		}()
	}
	close(start)
	first := <-results
	if first.err != nil {
		t.Fatal(first.err)
	}
	select {
	case second := <-results:
		t.Fatalf("two leaders returned: %v", second.err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	for range 2 {
		select {
		case candidate := <-results:
			if !errors.Is(candidate.err, context.Canceled) {
				t.Errorf("cancel waiting candidate: %v", candidate.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("candidate did not stop watching")
		}
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanupCancel()
	_ = first.leader.close(cleanupCtx)
}
