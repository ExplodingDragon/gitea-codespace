// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestLeaderExecutionHandoverPreservesGateway(t *testing.T) {
	service := &appE2EManagerService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()
	config := DefaultConfig()
	config.provisionerKind = "dummy"
	config.Node.PollInterval = Duration(20 * time.Millisecond)
	config.Node.ShutdownTimeout = Duration(3 * time.Second)
	config.Gateway.HTTP.Listen = "127.0.0.1:0"
	config.Gateway.SSH.Listen = "127.0.0.1:0"
	state := appE2ERuntimeConfig(t, config, ManagerSite{ID: 1, ManagerID: 1, GiteaURL: server.URL, ManagerSecret: "manager-secret"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gatewayCtx, stopGateway := context.WithCancel(ctx)
	gatewayDone := make(chan error, 1)
	output := newSignalOutput("gateway ssh listening")
	go func() { gatewayDone <- runGatewayConfig(gatewayCtx, output, state) }()
	t.Cleanup(func() {
		stopGateway()
		if gatewayDone != nil {
			if err := <-gatewayDone; err != nil {
				t.Error(err)
			}
		}
	})
	output.wait(t)
	firstCtx, stopFirst := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	state.NodeID = "first"
	go func() { firstDone <- runLeaderLoop(firstCtx, io.Discard, state) }()
	t.Cleanup(func() {
		stopFirst()
		if firstDone != nil {
			<-firstDone
		}
	})
	waitOwner := func(name string) {
		t.Helper()
		for ctx.Err() == nil {
			response, err := state.store.client.Get(ctx, state.store.key("leader"))
			if err == nil && len(response.Kvs) == 1 && strings.HasPrefix(string(response.Kvs[0].Value), name+"/") {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("leader %s was not elected", name)
	}
	waitOwner("first")
	for !service.sawFetch() && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if !service.sawFetch() {
		t.Fatal("leader did not recover and fetch")
	}
	second := state
	second.NodeID = "second"
	secondCtx, stopSecond := context.WithCancel(ctx)
	secondDone := make(chan error, 1)
	go func() { secondDone <- runLeaderLoop(secondCtx, io.Discard, second) }()
	t.Cleanup(func() {
		stopSecond()
		if err := <-secondDone; err != nil {
			t.Error(err)
		}
	})
	stopFirst()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	firstDone = nil
	waitOwner("second")
	select {
	case err := <-gatewayDone:
		gatewayDone = nil
		t.Fatalf("gateway stopped during handover: %v", err)
	default:
	}
}

func TestSharedGatewayActivity(t *testing.T) {
	state := newTestCodespaceStateStore(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	sessions := newGatewaySessionRegistryFromConfig(DefaultConfig().Gateway)
	const runtimeUUID = "11111111-1111-4111-8111-111111111111"
	sessions.mu.Lock()
	sessions.anonymousLive[runtimeUUID] = 1
	sessions.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); state.records.store.publishGatewayActivity(ctx, sessions) }()
	defer func() { cancel(); <-done }()
	tracker := &sharedGatewayActivity{store: state.records.store}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tracker.LiveSessions(runtimeUUID) == 1 && tracker.available {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !tracker.available {
		t.Fatal("gateway activity was not published")
	}
	if tracker.LiveSessions("other") != 0 {
		t.Fatal("activity was attributed to another runtime")
	}
	cancel()
	<-done
	tracker.nextRead = time.Time{}
	if tracker.LiveSessions("other") != 1 {
		t.Fatal("missing gateway observation permitted auto-stop")
	}
}

func TestLeaderRecoversOnlyAfterElection(t *testing.T) {
	config := DefaultConfig()
	config.provisionerKind = "dummy"
	state := appE2ERuntimeConfig(t, config, ManagerSite{ID: 1, ManagerID: 1, GiteaURL: "http://gitea.example.test", ManagerSecret: "secret"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := state.store.waitForLeadership(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.close(ctx) }()
	records := &executionStateStore{store: state.store, leader: first, siteID: 1}
	if err := records.save("runtimes/invalid-uuid", []byte("invalid checkpoint"), 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runLeaderLoop(ctx, io.Discard, state) }()
	select {
	case err := <-done:
		t.Fatalf("candidate attempted recovery before election: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := first.close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "invalid runtime checkpoint") {
			t.Fatalf("elected recovery: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("elected node did not attempt recovery")
	}
}
