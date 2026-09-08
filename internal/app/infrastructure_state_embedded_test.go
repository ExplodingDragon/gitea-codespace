// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedServiceConfigurationAndShutdown(t *testing.T) {
	setInfrastructureStateEnv(t, t.TempDir())
	t.Setenv(managerAdminListenEnv, "127.0.0.1:0")
	t.Setenv(managerAdminTokenEnv, "admin-token")
	output := newSignalOutput("manager admin listening")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runService(ctx, output) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("service did not stop")
		}
	}()
	output.wait(t)
	address := strings.TrimSpace(strings.TrimPrefix(output.String(), "codespace manager admin listening on "))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("admin", "admin-token")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("configuration page: %d", response.StatusCode)
	}
}

func TestManagerSecretCodecRejectsDamagedNonce(t *testing.T) {
	setInfrastructureStateEnv(t, t.TempDir())
	codec, err := newManagerSecretCodec()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.decrypt(`{"Nonce":"AA","Data":"AA"}`); err == nil {
		t.Fatal("damaged ciphertext accepted")
	}
}

func TestSharedGatewayRouteWatch(t *testing.T) {
	setInfrastructureStateEnv(t, t.TempDir())
	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	siteID, err := store.UpsertSite(ctx, UpsertAdminSiteOptions{
		GiteaURL: "https://gitea.example.com", ManagerID: 1, ManagerSecret: "secret", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	uuid := "11111111-1111-4111-8111-111111111111"
	if err := store.SaveRuntimeBinding(ctx, RuntimeBinding{
		RuntimeUUID: uuid, SiteID: siteID, BackendID: "incus", CodespaceID: 1, OperationRVersion: 1, EnvironmentTag: "standard",
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := GatewayRuntimeSnapshot{
		RuntimeUUID: uuid, SiteID: siteID, InstanceName: "runtime-1", Workdir: "/workspaces/repo",
		UID: 1000, GID: 1000, ContainerID: "container-1", ContainerUser: "developer",
		ContainerWorkdir: "/workspaces/repo", EditorPort: 13337,
		Endpoints: []GatewayEndpointSnapshot{{EndpointID: "workspace", Label: "Workspace", UpstreamPort: 13337}},
	}
	first, second := newGatewayRouteStore(), newGatewayRouteStore()
	defer first.Close()
	defer second.Close()
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); store.WatchGatewayRuntimes(watchCtx, first) }()
	defer func() { stop(); <-done }()
	secondDone := make(chan struct{})
	go func() { defer close(secondDone); store.WatchGatewayRuntimes(ctx, second) }()
	defer func() { cancel(); <-secondDone }()
	wait := func(routes *gatewayRouteStore, label string, exists bool) {
		t.Helper()
		for {
			route, ok := routes.Get(uuid, "workspace")
			if ok == exists && (!exists || route.label == label) {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("shared routes did not converge")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	if err := store.SaveGatewayRuntime(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	wait(first, "Workspace", true)
	wait(second, "Workspace", true)
	stop()
	<-done
	snapshot.Endpoints[0].Label = "Editor"
	if err := store.SaveGatewayRuntime(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	// A restarted gateway obtains changes made while it was disconnected.
	restartedCtx, stopRestarted := context.WithCancel(ctx)
	restartedDone := make(chan struct{})
	go func() { defer close(restartedDone); store.WatchGatewayRuntimes(restartedCtx, first) }()
	defer func() { stopRestarted(); <-restartedDone }()
	wait(first, "Editor", true)
	wait(second, "Editor", true)
	target, ok, err := first.LoadGatewayWorkspaceTarget(uuid)
	if err != nil || !ok || target.uid != 1000 || target.workdir != "/workspaces/repo" {
		t.Fatal("workspace identity not propagated")
	}
	if err := store.DeleteRuntimeBinding(ctx, uuid); err != nil {
		t.Fatal(err)
	}
	wait(first, "", false)
	wait(second, "", false)
}
