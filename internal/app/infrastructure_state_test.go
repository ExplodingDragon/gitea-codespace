// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/etcdutl/v3/snapshot"
	"go.etcd.io/etcd/server/v3/embed"
	"go.uber.org/zap"
)

func TestInfrastructureStatePersistsConfigAndEncryptedSiteSecret(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "etcd")
	setInfrastructureStateEnv(t, statePath)

	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer func() { _ = store.Close() }()

	config := DefaultConfig()
	if err := store.SaveConfigOnly(context.Background(), config); err != nil {
		t.Fatalf("save config: %v", err)
	}
	site := UpsertAdminSiteOptions{GiteaURL: "https://gitea.example.com", ManagerID: 42, ManagerSecret: "plain-manager-secret", Enabled: true}
	if _, err := store.UpsertSite(context.Background(), site); err != nil {
		t.Fatalf("save site: %v", err)
	}

	if len(store.embedded.Clients) != 0 || len(store.embedded.Peers) != 0 {
		t.Fatal("embedded store opened network listeners")
	}
	if duplicate, err := openEmbeddedInfrastructureStore(); err == nil {
		_ = duplicate.Close()
		t.Fatal("duplicate data directory owner was accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("load runtime config: %v", err)
	}
	if len(loaded.Sites) != 1 || loaded.Sites[0].GiteaURL != site.GiteaURL ||
		loaded.Sites[0].ManagerID != site.ManagerID ||
		loaded.Sites[0].ManagerSecret != site.ManagerSecret ||
		loaded.Config.Node.CapacityTotal != config.Node.CapacityTotal {
		t.Fatalf("loaded runtime config = %#v", loaded)
	}
	content, err := os.ReadFile(filepath.Join(statePath, "member", "snap", "db"))
	if err != nil {
		t.Fatalf("read state database: %v", err)
	}
	if bytes.Contains(content, []byte(site.ManagerSecret)) {
		t.Fatalf("state database contains plaintext manager secret")
	}
	request := httptest.NewRequest(http.MethodGet, "/api/state/snapshot", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	response := httptest.NewRecorder()
	newInfrastructureAdminHandler(store, "admin-token").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot: %d %s", response.Code, response.Body.String())
	}
	backup := filepath.Join(t.TempDir(), "backup.snapshot")
	if err := os.WriteFile(backup, response.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	restoredDir := filepath.Join(t.TempDir(), "restored")
	if err := snapshot.NewV3(zap.NewNop()).Restore(snapshot.RestoreConfig{
		SnapshotPath: backup, OutputDataDir: restoredDir, Name: "codespace",
		PeerURLs:       []string{"http://127.0.0.1:2380"},
		InitialCluster: "codespace=http://127.0.0.1:2380", InitialClusterToken: "etcd-cluster",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(managerStatePathEnv, restoredDir)
	store, err = openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := store.LoadRuntimeConfig(context.Background())
	if err != nil || len(restored.Sites) != 1 || restored.Sites[0].ManagerSecret != site.ManagerSecret {
		t.Fatalf("restored identity: %v", err)
	}
}

func TestInfrastructureStateLoadsEnabledSitesAndPreservesSecret(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "etcd")
	setInfrastructureStateEnv(t, statePath)
	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer func() { _ = store.Close() }()

	config := DefaultConfig()
	if err := store.SaveConfigOnly(context.Background(), config); err != nil {
		t.Fatalf("save config: %v", err)
	}
	firstID, err := store.UpsertSite(context.Background(), UpsertAdminSiteOptions{
		GiteaURL: "https://one.example.com", ManagerID: 1, ManagerSecret: "first-secret", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create first site: %v", err)
	}
	if _, err := store.UpsertSite(context.Background(), UpsertAdminSiteOptions{
		GiteaURL: "https://two.example.com", ManagerID: 2, ManagerSecret: "second-secret", Enabled: true,
	}); err != nil {
		t.Fatalf("create second site: %v", err)
	}
	if _, err := store.UpsertSite(context.Background(), UpsertAdminSiteOptions{
		ID: firstID, GiteaURL: "https://one.example.com", ManagerID: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("update first site without replacing secret: %v", err)
	}

	loaded, err := store.LoadRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("load runtime config: %v", err)
	}
	if len(loaded.Sites) != 2 || loaded.Sites[0].ManagerSecret != "first-secret" || loaded.Sites[1].ManagerSecret != "second-secret" {
		t.Fatalf("loaded sites = %#v", loaded.Sites)
	}
	binding := RuntimeBinding{
		RuntimeUUID: "11111111-1111-4111-8111-111111111111", SiteID: firstID, BackendID: "incus",
		CodespaceID: 4, OperationRVersion: 2, EnvironmentTag: "standard",
	}
	if err := store.SaveRuntimeBinding(context.Background(), binding); err != nil {
		t.Fatalf("save runtime binding: %v", err)
	}
	missingSite := binding
	missingSite.RuntimeUUID = "33333333-3333-4333-8333-333333333333"
	missingSite.SiteID = 999
	if err := store.SaveRuntimeBinding(context.Background(), missingSite); err == nil {
		t.Fatal("runtime binding for a missing site was accepted")
	}
	conflicting := binding
	conflicting.SiteID++
	if err := store.SaveRuntimeBinding(context.Background(), conflicting); err == nil {
		t.Fatalf("cross-site runtime binding was accepted")
	}
	if err := store.DeleteSite(context.Background(), firstID); err == nil {
		t.Fatalf("site with a runtime binding was deleted")
	}
	if _, err := store.UpsertSite(context.Background(), UpsertAdminSiteOptions{
		ID: firstID, GiteaURL: "https://changed.example.com", ManagerID: 1, Enabled: true,
	}); err == nil {
		t.Fatalf("site identity with a runtime binding was changed")
	}
	bindings, err := store.ListRuntimeBindings(context.Background())
	if err != nil {
		t.Fatalf("list runtime bindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0] != binding {
		t.Fatalf("runtime bindings = %#v", bindings)
	}
	processState := processStateSnapshot{sites: []processSiteState{{site: ManagerSite{ID: firstID}, runtimeUUIDs: []string{binding.RuntimeUUID}}}}
	if err := validateProcessRuntimeBindings(context.Background(), processState, store); err != nil {
		t.Fatalf("validate runtime binding ownership: %v", err)
	}
	snapshot := GatewayRuntimeSnapshot{
		RuntimeUUID: binding.RuntimeUUID, SiteID: firstID, InstanceName: "runtime-1", Workdir: "/workspaces/repo",
		UID: 1000, GID: 1000, ContainerID: "container-1", ContainerUser: "developer",
		ContainerWorkdir: "/workspaces/repo", EditorPort: 13337,
		Endpoints: []GatewayEndpointSnapshot{{EndpointID: "workspace", Label: "Workspace", UpstreamPort: 13337}},
	}
	if err := store.SaveGatewayRuntime(context.Background(), snapshot); err != nil {
		t.Fatalf("save gateway runtime: %v", err)
	}
	snapshots, err := store.ListGatewayRuntimes(context.Background())
	if err != nil {
		t.Fatalf("list gateway runtimes: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].RuntimeUUID != snapshot.RuntimeUUID || snapshots[0].SiteID != firstID || len(snapshots[0].Endpoints) != 1 {
		t.Fatalf("gateway runtimes = %#v", snapshots)
	}
	if err := store.DeleteRuntimeBinding(context.Background(), binding.RuntimeUUID); err != nil {
		t.Fatalf("delete runtime binding: %v", err)
	}
	if err := validateProcessRuntimeBindings(context.Background(), processState, store); err == nil {
		t.Fatalf("runtime state without binding was accepted")
	}
	processState.sites[0].initialCleanupPendings = []string{binding.RuntimeUUID}
	if err := validateProcessRuntimeBindings(context.Background(), processState, store); err != nil {
		t.Fatalf("cleanup-pending runtime without binding was rejected: %v", err)
	}
	snapshots, err = store.ListGatewayRuntimes(context.Background())
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("gateway runtimes after binding delete = %#v err = %v", snapshots, err)
	}
}

func TestInfrastructureAdminSiteAPIHidesSecret(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "etcd")
	setInfrastructureStateEnv(t, statePath)
	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer func() { _ = store.Close() }()
	handler := newInfrastructureAdminHandler(store, "admin-token")

	body := strings.NewReader(`{"gitea_url":"https://gitea.example.com","manager_id":7,"manager_secret":"hidden-secret","enabled":false}`)
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

func TestInfrastructureAdminVerifiesEnabledSite(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "etcd")
	setInfrastructureStateEnv(t, statePath)
	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer func() { _ = store.Close() }()
	config := DefaultConfig()
	if err := store.SaveConfigOnly(context.Background(), config); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := &managerCheckService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()
	service.giteaURL = server.URL

	handler := newInfrastructureAdminHandler(store, "admin-token")
	body := strings.NewReader(fmt.Sprintf(`{"gitea_url":%q,"manager_id":7,"manager_secret":"manager-secret","enabled":true}`, server.URL))
	request := httptest.NewRequest(http.MethodPost, "/api/sites", body)
	request.Header.Set("Authorization", "Bearer admin-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.calls != 1 {
		t.Fatalf("create site status = %d calls = %d body = %s", response.Code, service.calls, response.Body.String())
	}
	loaded, err := store.LoadRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("load runtime config: %v", err)
	}
	if len(loaded.Sites) != 1 || loaded.Sites[0].GiteaURL != server.URL {
		t.Fatalf("loaded sites = %#v", loaded.Sites)
	}
}

type managerCheckService struct {
	codespacev1connect.UnimplementedManagerServiceHandler
	giteaURL string
	calls    int
}

func (s *managerCheckService) CheckManager(_ context.Context, request *connect.Request[codespacev1.CheckManagerRequest]) (*connect.Response[codespacev1.CheckManagerResponse], error) {
	s.calls++
	if request.Msg.GetProtocolVersion() != 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unexpected protocol version"))
	}
	return connect.NewResponse(&codespacev1.CheckManagerResponse{GiteaWebUrl: s.giteaURL, ManagerName: "Manager 1"}), nil
}

func TestInfrastructureAdminAPIRequiresBearerToken(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "etcd")
	setInfrastructureStateEnv(t, statePath)
	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer func() { _ = store.Close() }()
	handler := newInfrastructureAdminHandler(store, "admin-token")

	for _, target := range []string{"/api/sites", "/api/config", "/api/state/snapshot"} {
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
	if err := store.SaveConfigOnly(context.Background(), config); err != nil {
		t.Fatalf("save etcd config: %v", err)
	}
	site := UpsertAdminSiteOptions{GiteaURL: "https://gitea-etcd.example.com", ManagerID: 77, ManagerSecret: "plain-etcd-secret", Enabled: true}
	if _, err := store.UpsertSite(context.Background(), site); err != nil {
		t.Fatalf("save etcd site: %v", err)
	}

	loaded, err := store.LoadRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("load etcd runtime config: %v", err)
	}
	if len(loaded.Sites) != 1 || loaded.Sites[0].GiteaURL != site.GiteaURL ||
		loaded.Sites[0].ManagerID != site.ManagerID ||
		loaded.Sites[0].ManagerSecret != site.ManagerSecret ||
		loaded.Config.Node.CapacityTotal != config.Node.CapacityTotal {
		t.Fatalf("loaded etcd runtime config = %#v", loaded)
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
	binding := RuntimeBinding{
		RuntimeUUID: "11111111-1111-4111-8111-111111111111", SiteID: 1, BackendID: "incus",
		CodespaceID: 9, OperationRVersion: 1, EnvironmentTag: "default",
	}
	if err := store.SaveRuntimeBinding(context.Background(), binding); err != nil {
		t.Fatalf("save etcd runtime binding: %v", err)
	}
	missingSite := binding
	missingSite.RuntimeUUID = "33333333-3333-4333-8333-333333333333"
	missingSite.SiteID = 999
	if err := store.SaveRuntimeBinding(context.Background(), missingSite); err == nil {
		t.Fatal("etcd runtime binding for a missing site was accepted")
	}
	conflicting := binding
	conflicting.SiteID = 2
	if err := store.SaveRuntimeBinding(context.Background(), conflicting); err == nil {
		t.Fatal("cross-site etcd runtime binding was accepted")
	}
	snapshot := GatewayRuntimeSnapshot{
		RuntimeUUID: binding.RuntimeUUID, SiteID: 1, InstanceName: "runtime-1", Workdir: "/workspaces/repo",
		UID: 1000, GID: 1000, ContainerID: "container-1", ContainerUser: "developer",
		ContainerWorkdir: "/workspaces/repo", EditorPort: 13337,
		Endpoints: []GatewayEndpointSnapshot{{EndpointID: "workspace", UpstreamPort: 13337}},
	}
	if err := store.SaveGatewayRuntime(context.Background(), snapshot); err != nil {
		t.Fatalf("save etcd gateway runtime: %v", err)
	}
	snapshots, err := store.ListGatewayRuntimes(context.Background())
	if err != nil || len(snapshots) != 1 || snapshots[0].SiteID != 1 {
		t.Fatalf("etcd gateway runtimes = %#v err = %v", snapshots, err)
	}
	if err := store.DeleteSite(context.Background(), 1); err == nil {
		t.Fatal("etcd site with a runtime binding was deleted")
	}
	if err := store.DeleteGatewayRuntime(context.Background(), binding.RuntimeUUID); err != nil {
		t.Fatalf("delete etcd gateway runtime: %v", err)
	}
	if err := store.DeleteRuntimeBinding(context.Background(), binding.RuntimeUUID); err != nil {
		t.Fatalf("delete etcd runtime binding: %v", err)
	}

	workerContext, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	leader, err := store.waitForLeadership(workerContext, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = leader.close(context.Background()) }()
	secondStore, err := openEtcdInfrastructureStore()
	if err != nil {
		t.Fatalf("open second etcd store: %v", err)
	}
	defer func() { _ = secondStore.Close() }()
	firstKey, err := store.LoadGatewaySSHHostKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := secondStore.LoadGatewaySSHHostKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if firstKey.fingerprintSHA256 != secondKey.fingerprintSHA256 {
		t.Fatal("gateway nodes have different SSH identities")
	}
	candidateCtx, cancelCandidate := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelCandidate()
	_, err = secondStore.waitForLeadership(candidateCtx, "node-2")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting candidate: %v", err)
	}

	siteID, err := store.UpsertSite(context.Background(), UpsertAdminSiteOptions{
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
	if err := store.SaveConfigOnly(context.Background(), config); err != nil {
		t.Fatalf("save cluster config: %v", err)
	}
	site := UpsertAdminSiteOptions{GiteaURL: "https://gitea-cluster.example.com", ManagerID: 79, ManagerSecret: "cluster-secret", Enabled: true}
	if _, err := store.UpsertSite(context.Background(), site); err != nil {
		t.Fatalf("save cluster site: %v", err)
	}
	loaded, err := store.LoadRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("load cluster runtime config: %v", err)
	}
	if len(loaded.Sites) != 1 || loaded.Sites[0].ManagerSecret != site.ManagerSecret {
		t.Fatalf("cluster sites = %#v", loaded.Sites)
	}
}

func TestGatewayServiceSkipsWorkerRPC(t *testing.T) {
	setInfrastructureStateEnv(t, t.TempDir())
	store, err := openEmbeddedInfrastructureStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	stateDir := filepath.Join(t.TempDir(), "state")
	stateLock, err := acquireStateDirLock(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stateLock.Close() }()
	service := &lockTestManagerService{}
	server := newGiteaManagerServiceServer(t, service)
	defer server.Close()
	managerState := ManagerSite{ID: 80, GiteaURL: server.URL, ManagerID: 80, ManagerSecret: "manager-secret"}

	output := newSignalOutput("gateway ssh listening")
	config := DefaultConfig()
	config.Node.HTTPTimeout = Duration(100 * time.Millisecond)
	config.Node.ShutdownTimeout = Duration(time.Second)
	config.Gateway.HTTP.Listen = "127.0.0.1:0"
	config.Gateway.SSH.Listen = "127.0.0.1:0"
	config.provisionerKind = "dummy"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runGatewayConfig(ctx, output, InfrastructureRuntimeConfig{
			Config: config,
			Sites:  []ManagerSite{managerState},
			store:  store,
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
	t.Setenv(managerStateDriverEnv, "embedded")
	t.Setenv(managerStatePathEnv, statePath)
	t.Setenv(managerStateEncryptionKeyEnv, base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
}

func openEtcdInfrastructureStoreForTest(t *testing.T, endpointEnv string) *etcdInfrastructureStore {
	t.Helper()

	endpoints := strings.TrimSpace(os.Getenv(endpointEnv))
	if endpoints == "" {
		if endpointEnv != "CODESPACE_TEST_ETCD_ENDPOINTS" {
			t.Skipf("%s is not set", endpointEnv)
		}
		// Exercise the external client against a real server without an installed binary.
		cfg := embed.NewConfig()
		cfg.Dir = t.TempDir()
		cfg.ListenPeerUrls = nil
		cfg.ListenClientUrls = []url.URL{{Scheme: "http", Host: "127.0.0.1:0"}}
		cfg.AdvertiseClientUrls = cfg.ListenClientUrls
		cfg.LogLevel = "error"
		server, err := embed.StartEtcd(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(server.Close)
		select {
		case <-server.Server.ReadyNotify():
		case <-time.After(15 * time.Second):
			t.Fatal("test etcd did not become ready")
		}
		endpoints = "http://" + server.Clients[0].Addr().String()
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
