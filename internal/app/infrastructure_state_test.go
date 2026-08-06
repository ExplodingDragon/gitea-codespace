// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestInfrastructureStatePersistsConfigAndEncryptedSiteSecret(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "manager.db")
	setInfrastructureStateEnv(t, statePath)

	store, err := openLocalInfrastructureStore()
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer func() { _ = store.Close() }()

	config := DefaultConfig()
	config.Node.StateDir = filepath.Join(t.TempDir(), "state")
	managerState := ManagerState{
		GiteaURL:            "https://gitea.example.com",
		ManagerID:           42,
		ManagerSecret:       "plain-manager-secret",
		InventoryGeneration: 0,
	}
	if err := store.SaveRuntimeConfig(context.Background(), config, managerState); err != nil {
		t.Fatalf("save runtime config: %v", err)
	}

	loaded, err := store.LoadRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("load runtime config: %v", err)
	}
	if loaded.ManagerState.GiteaURL != managerState.GiteaURL ||
		loaded.ManagerState.ManagerID != managerState.ManagerID ||
		loaded.ManagerState.ManagerSecret != managerState.ManagerSecret ||
		loaded.Config.Node.StateDir != config.Node.StateDir {
		t.Fatalf("loaded runtime config = %#v", loaded)
	}
	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state database: %v", err)
	}
	if bytes.Contains(content, []byte(managerState.ManagerSecret)) {
		t.Fatalf("state database contains plaintext manager secret")
	}
}

func TestInfrastructureAdminSiteAPIHidesSecret(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "manager.db")
	setInfrastructureStateEnv(t, statePath)
	store, err := openLocalInfrastructureStore()
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer func() { _ = store.Close() }()
	handler := newInfrastructureAdminHandler(store, "admin-token")

	body := strings.NewReader(`{"name":"primary","gitea_url":"https://gitea.example.com","manager_id":7,"manager_secret":"hidden-secret","enabled":true}`)
	request := httptest.NewRequest(http.MethodPost, "/api/sites", body)
	request.Header.Set("Authorization", "Bearer admin-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create site status = %d body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list site status = %d body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "hidden-secret") || !strings.Contains(response.Body.String(), "gitea.example.com") {
		t.Fatalf("unexpected site response: %s", response.Body.String())
	}
}

func TestInfrastructureAdminAPIRequiresBearerToken(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "manager.db")
	setInfrastructureStateEnv(t, statePath)
	store, err := openLocalInfrastructureStore()
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer func() { _ = store.Close() }()
	handler := newInfrastructureAdminHandler(store, "admin-token")

	for _, target := range []string{"/api/sites", "/api/config"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token status = %d body = %s", target, response.Code, response.Body.String())
		}

		request = httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer wrong-token")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s with wrong token status = %d body = %s", target, response.Code, response.Body.String())
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestEtcdInfrastructureStateSingleNode(t *testing.T) {
	store := openEtcdInfrastructureStoreForTest(t, "CODESPACE_TEST_ETCD_ENDPOINTS")
	defer closeEtcdInfrastructureStoreForTest(t, store)

	config := DefaultConfig()
	config.Node.StateDir = filepath.Join(t.TempDir(), "state")
	managerState := ManagerState{
		GiteaURL:      "https://gitea-etcd.example.com",
		ManagerID:     77,
		ManagerSecret: "plain-etcd-secret",
	}
	if err := store.SaveRuntimeConfig(context.Background(), config, managerState); err != nil {
		t.Fatalf("save etcd runtime config: %v", err)
	}

	loaded, err := store.LoadRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("load etcd runtime config: %v", err)
	}
	if loaded.ManagerState.GiteaURL != managerState.GiteaURL ||
		loaded.ManagerState.ManagerID != managerState.ManagerID ||
		loaded.ManagerState.ManagerSecret != managerState.ManagerSecret ||
		loaded.Config.Node.StateDir != config.Node.StateDir {
		t.Fatalf("loaded etcd runtime config = %#v", loaded)
	}
	managerState.GiteaURL = "https://gitea-etcd-renamed.example.com"
	if err := store.SaveRuntimeConfig(context.Background(), config, managerState); err != nil {
		t.Fatalf("update etcd runtime config: %v", err)
	}
	oldIdentitySiteID, err := store.UpsertSite(context.Background(), UpsertAdminSiteOptions{
		Name:          "old-default-identity",
		GiteaURL:      "https://gitea-etcd.example.com",
		ManagerID:     77,
		ManagerSecret: "old-default-secret",
		Enabled:       false,
	})
	if err != nil {
		t.Fatalf("reuse old default etcd site identity: %v", err)
	}
	if err := store.DeleteSite(context.Background(), oldIdentitySiteID); err != nil {
		t.Fatalf("delete old default etcd site identity: %v", err)
	}
	resp, err := store.client.Get(context.Background(), store.prefix+"/", clientv3.WithPrefix())
	if err != nil {
		t.Fatalf("read raw etcd state: %v", err)
	}
	for _, kv := range resp.Kvs {
		if bytes.Contains(kv.Value, []byte("plain-etcd-secret")) {
			t.Fatalf("etcd state contains plaintext manager secret at %s", string(kv.Key))
		}
	}

	siteID, err := store.UpsertSite(context.Background(), UpsertAdminSiteOptions{
		Name:          "secondary",
		GiteaURL:      "https://gitea-secondary.example.com",
		ManagerID:     78,
		ManagerSecret: "secondary-secret",
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("create etcd site: %v", err)
	}
	if siteID <= 1 {
		t.Fatalf("created etcd site id = %d", siteID)
	}
	if _, err := store.UpsertSite(context.Background(), UpsertAdminSiteOptions{
		Name:          "duplicate",
		GiteaURL:      "https://gitea-secondary.example.com",
		ManagerID:     78,
		ManagerSecret: "duplicate-secret",
		Enabled:       true,
	}); err == nil {
		t.Fatal("duplicate etcd manager site was accepted")
	}
	sites, err := store.ListSites(context.Background())
	if err != nil {
		t.Fatalf("list etcd sites: %v", err)
	}
	if len(sites) != 2 || sites[0].ID != 1 || sites[1].ID != siteID {
		t.Fatalf("etcd sites = %#v", sites)
	}
	if err := store.DeleteSite(context.Background(), siteID); err != nil {
		t.Fatalf("delete etcd site: %v", err)
	}
	if _, err := store.UpsertSite(context.Background(), UpsertAdminSiteOptions{
		Name:          "secondary-recreated",
		GiteaURL:      "https://gitea-secondary.example.com",
		ManagerID:     78,
		ManagerSecret: "secondary-secret",
		Enabled:       true,
	}); err != nil {
		t.Fatalf("recreate deleted etcd site: %v", err)
	}
}

func TestEtcdInfrastructureStateClusterEndpoints(t *testing.T) {
	store := openEtcdInfrastructureStoreForTest(t, "CODESPACE_TEST_ETCD_CLUSTER_ENDPOINTS")
	defer closeEtcdInfrastructureStoreForTest(t, store)

	config := DefaultConfig()
	config.Node.StateDir = filepath.Join(t.TempDir(), "state")
	managerState := ManagerState{
		GiteaURL:      "https://gitea-cluster.example.com",
		ManagerID:     79,
		ManagerSecret: "cluster-secret",
	}
	if err := store.SaveRuntimeConfig(context.Background(), config, managerState); err != nil {
		t.Fatalf("save cluster runtime config: %v", err)
	}
	loaded, err := store.LoadRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("load cluster runtime config: %v", err)
	}
	if loaded.ManagerState.ManagerSecret != managerState.ManagerSecret {
		t.Fatalf("cluster manager secret = %q", loaded.ManagerState.ManagerSecret)
	}
}

func TestRunWithConfigGatewayRoleSkipsWorkerRPC(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	service := &lockTestManagerService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()
	managerState := saveManagerStateForTest(t, stateDir, server.URL, 80)

	output := newSignalOutput("gateway ssh listening")
	config := DefaultConfig()
	config.Node.StateDir = stateDir
	config.Node.HTTPTimeout = Duration(100 * time.Millisecond)
	config.Node.ShutdownTimeout = Duration(time.Second)
	config.Gateway.HTTP.Listen = "127.0.0.1:0"
	config.Gateway.SSH.Listen = "127.0.0.1:0"
	config.provisionerKind = "dummy"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithConfigContext(ctx, output, InfrastructureRuntimeConfig{
			Config:       config,
			ManagerState: managerState,
			NodeRole:     managerNodeRoleGateway,
		})
	}()
	output.wait(t)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gateway-only run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway-only run did not stop")
	}
	if service.calls.Load() != 0 {
		t.Fatalf("manager service calls = %d", service.calls.Load())
	}
}

func setInfrastructureStateEnv(t *testing.T, statePath string) {
	t.Helper()
	t.Setenv(managerStateDriverEnv, "local")
	t.Setenv(managerStatePathEnv, statePath)
	t.Setenv(managerStateEncryptionKeyEnv, base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
}

func openEtcdInfrastructureStoreForTest(t *testing.T, endpointEnv string) *etcdInfrastructureStore {
	t.Helper()

	endpoints := strings.TrimSpace(os.Getenv(endpointEnv))
	if endpoints == "" {
		t.Skipf("%s is not set", endpointEnv)
	}
	t.Setenv(managerStateDriverEnv, "etcd")
	t.Setenv(managerStateEtcdEndpointsEnv, endpoints)
	t.Setenv(managerStateEtcdPrefixEnv, fmt.Sprintf("/gitea-codespace-test-%d", time.Now().UnixNano()))
	t.Setenv(managerStateEncryptionKeyEnv, base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	store, err := openEtcdInfrastructureStore()
	if err != nil {
		t.Fatalf("open etcd state store: %v", err)
	}
	return store
}

func closeEtcdInfrastructureStoreForTest(t *testing.T, store *etcdInfrastructureStore) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := store.client.Delete(ctx, store.prefix+"/", clientv3.WithPrefix()); err != nil {
		t.Fatalf("clean etcd state: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close etcd state store: %v", err)
	}
}

type signalOutput struct {
	mu     sync.Mutex
	needle string
	done   chan struct{}
	closed bool
	text   strings.Builder
}

func newSignalOutput(needle string) *signalOutput {
	return &signalOutput{needle: needle, done: make(chan struct{})}
}

func (w *signalOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.text.Write(p)
	if !w.closed && strings.Contains(w.text.String(), w.needle) {
		close(w.done)
		w.closed = true
	}
	return n, err
}

func (w *signalOutput) wait(t *testing.T) {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("output did not contain %q; output = %s", w.needle, w.String())
	}
}

func (w *signalOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.String()
}
