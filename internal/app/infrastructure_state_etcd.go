// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
)

const defaultManagerStateEtcdPrefix = "/gitea-codespace"

type etcdInfrastructureStore struct {
	embedded   *embed.Etcd
	stateLock  *stateDirLock
	closeOnce  sync.Once
	closeErr   error
	client     *clientv3.Client
	prefix     string
	secret     managerSecretCodec
	leadership *deploymentLeadership
}

type etcdSiteRecord struct {
	ID            int64  `json:"id"`
	GiteaURL      string `json:"gitea_url"`
	ManagerID     int64  `json:"manager_id"`
	ManagerSecret string `json:"manager_secret"`
	Enabled       bool   `json:"enabled"`
}

func openEtcdInfrastructureStore() (*etcdInfrastructureStore, error) {
	codec, err := newManagerSecretCodec()
	if err != nil {
		return nil, err
	}
	endpoints, err := managerStateEtcdEndpoints()
	if err != nil {
		return nil, err
	}
	prefix, err := managerStateEtcdPrefix()
	if err != nil {
		return nil, err
	}
	clientConfig := clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
		Username:    os.Getenv("GITEA_CODESPACE_ETCD_USERNAME"),
		Password:    os.Getenv("GITEA_CODESPACE_ETCD_PASSWORD"),
	}
	if (clientConfig.Username == "") != (clientConfig.Password == "") {
		return nil, fmt.Errorf("etcd username and password must be configured together")
	}
	caFile := strings.TrimSpace(os.Getenv("GITEA_CODESPACE_ETCD_CA"))
	certFile := strings.TrimSpace(os.Getenv("GITEA_CODESPACE_ETCD_CERT"))
	keyFile := strings.TrimSpace(os.Getenv("GITEA_CODESPACE_ETCD_KEY"))
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("etcd certificate and key must be configured together")
	}
	if caFile != "" || certFile != "" {
		clientConfig.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
		if caFile != "" {
			data, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf("read etcd CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(data) {
				return nil, fmt.Errorf("etcd CA contains no certificates")
			}
			clientConfig.TLS.RootCAs = pool
		}
		if certFile != "" {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, fmt.Errorf("load etcd client certificate: %w", err)
			}
			clientConfig.TLS.Certificates = []tls.Certificate{cert}
		}
	}
	if clientConfig.Username != "" || clientConfig.TLS != nil {
		for _, endpoint := range endpoints {
			if !strings.HasPrefix(endpoint, "https://") {
				return nil, fmt.Errorf("authenticated etcd connections require HTTPS endpoints")
			}
		}
	}
	client, err := clientv3.New(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("open manager etcd state: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Get(ctx, prefix+"/", clientv3.WithLimit(1)); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect manager etcd state: %w", err)
	}
	return &etcdInfrastructureStore{client: client, prefix: prefix, secret: codec}, nil
}

func managerStateEtcdEndpoints() ([]string, error) {
	raw := strings.Split(strings.TrimSpace(os.Getenv(managerStateEtcdEndpointsEnv)), ",")
	endpoints := make([]string, 0, len(raw))
	for _, endpoint := range raw {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint != "" {
			endpoints = append(endpoints, endpoint)
		}
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("%s is required for external etcd", managerStateEtcdEndpointsEnv)
	}
	return endpoints, nil
}

func managerStateEtcdPrefix() (string, error) {
	prefix := strings.TrimSpace(os.Getenv(managerStateEtcdPrefixEnv))
	if prefix == "" {
		return defaultManagerStateEtcdPrefix, nil
	}
	if !strings.HasPrefix(prefix, "/") {
		return "", fmt.Errorf("%s must start with /", managerStateEtcdPrefixEnv)
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return "", fmt.Errorf("%s must not be /", managerStateEtcdPrefixEnv)
	}
	return prefix, nil
}

func (s *etcdInfrastructureStore) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		closeErr := s.client.Close()
		// The in-process client has no connection; Close returns its canceled context.
		if s.embedded != nil && errors.Is(closeErr, context.Canceled) {
			closeErr = nil
		}
		s.closeErr = errors.Join(s.closeErr, closeErr)
		if s.embedded != nil {
			s.embedded.Close()
		}
		if s.stateLock != nil {
			s.closeErr = errors.Join(s.closeErr, s.stateLock.Close())
		}
	})
	return s.closeErr
}

func (s *etcdInfrastructureStore) LoadRuntimeConfig(ctx context.Context) (InfrastructureRuntimeConfig, error) {
	config, err := s.LoadConfigOnly(ctx)
	if err != nil {
		return InfrastructureRuntimeConfig{}, err
	}
	sites, err := s.loadEnabledSites(ctx)
	if err != nil {
		return InfrastructureRuntimeConfig{}, err
	}
	return InfrastructureRuntimeConfig{
		Config: config,
		Sites:  sites,
		NodeID: managerNodeID(),
	}, nil
}

func (s *etcdInfrastructureStore) SaveConfigOnly(ctx context.Context, config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode manager runtime config: %w", err)
	}
	if _, err := s.client.Put(ctx, s.key("config"), string(configJSON)); err != nil {
		return fmt.Errorf("save manager runtime config: %w", err)
	}
	return nil
}

func (s *etcdInfrastructureStore) LoadConfigOnly(ctx context.Context) (Config, error) {
	resp, err := s.client.Get(ctx, s.key("config"))
	if err != nil {
		return Config{}, fmt.Errorf("load manager runtime config: %w", err)
	}
	if len(resp.Kvs) == 0 {
		return Config{}, errInfrastructureStateEmpty
	}
	config := DefaultConfig()
	if err := json.Unmarshal(resp.Kvs[0].Value, &config); err != nil {
		return Config{}, fmt.Errorf("decode manager runtime config: %w", err)
	}
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate manager runtime config from state: %w", err)
	}
	return config, nil
}

func (s *etcdInfrastructureStore) ListSites(ctx context.Context) ([]AdminSite, error) {
	records, err := s.listSiteRecords(ctx)
	if err != nil {
		return nil, err
	}
	sites := make([]AdminSite, 0, len(records))
	for _, record := range records {
		sites = append(sites, AdminSite{
			ID:        record.ID,
			GiteaURL:  record.GiteaURL,
			ManagerID: record.ManagerID,
			Enabled:   record.Enabled,
		})
	}
	return sites, nil
}

func (s *etcdInfrastructureStore) LoadSite(ctx context.Context, id int64) (ManagerSite, error) {
	record, _, err := s.loadSiteRecord(ctx, id)
	if err != nil {
		return ManagerSite{}, err
	}
	if record.ID == 0 {
		return ManagerSite{}, fmt.Errorf("manager site %d does not exist", id)
	}
	secret, err := s.secret.decrypt(record.ManagerSecret)
	if err != nil {
		return ManagerSite{}, err
	}
	return ManagerSite{
		ID:            record.ID,
		GiteaURL:      record.GiteaURL,
		ManagerID:     record.ManagerID,
		ManagerSecret: secret,
	}, nil
}

func (s *etcdInfrastructureStore) UpsertSite(ctx context.Context, opts UpsertAdminSiteOptions) (int64, error) {
	giteaURL, err := normalizeGiteaURL(opts.GiteaURL)
	if err != nil {
		return 0, err
	}
	if opts.ManagerID <= 0 {
		return 0, fmt.Errorf("manager_id must be a positive integer")
	}
	if opts.ID <= 0 && strings.TrimSpace(opts.ManagerSecret) == "" {
		return 0, fmt.Errorf("manager_secret is required")
	}
	id := opts.ID
	if id <= 0 {
		id, err = s.nextSiteID(ctx)
		if err != nil {
			return 0, err
		}
	} else if err := s.ensureSiteSequenceAtLeast(ctx, id); err != nil {
		return 0, err
	}
	var secret string
	current, _, err := s.loadSiteRecord(ctx, id)
	if err != nil {
		return 0, err
	}
	if current.ID > 0 && (current.GiteaURL != giteaURL || current.ManagerID != opts.ManagerID) {
		hasBindings, err := s.siteHasRuntimeBindings(ctx, id)
		if err != nil {
			return 0, err
		}
		if hasBindings {
			return 0, fmt.Errorf("manager site %d owns runtimes and its Gitea identity cannot be changed", id)
		}
	}
	if strings.TrimSpace(opts.ManagerSecret) == "" {
		if current.ID == 0 {
			return 0, fmt.Errorf("manager site %d does not exist", id)
		}
		secret = current.ManagerSecret
	} else {
		secret, err = s.secret.encrypt(strings.TrimSpace(opts.ManagerSecret))
		if err != nil {
			return 0, err
		}
	}
	record := etcdSiteRecord{
		ID:            id,
		GiteaURL:      giteaURL,
		ManagerID:     opts.ManagerID,
		ManagerSecret: secret,
		Enabled:       opts.Enabled,
	}
	if err := s.saveSiteRecord(ctx, record); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *etcdInfrastructureStore) saveSiteRecord(ctx context.Context, record etcdSiteRecord, extraOps ...clientv3.Op) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode manager site: %w", err)
	}
	id := record.ID
	giteaURL := record.GiteaURL
	managerID := record.ManagerID
	siteKey := s.siteKey(id)
	uniqueKey := s.siteUniqueKey(giteaURL, managerID)
	for {
		current, siteRevision, err := s.loadSiteRecord(ctx, id)
		if err != nil {
			return err
		}
		if current.ID > 0 && (current.GiteaURL != record.GiteaURL || current.ManagerID != record.ManagerID) {
			hasBindings, err := s.siteHasRuntimeBindings(ctx, id)
			if err != nil {
				return err
			}
			if hasBindings {
				return fmt.Errorf("manager site %d owns runtimes and its Gitea identity cannot be changed", id)
			}
		}
		uniqueResp, err := s.client.Get(ctx, uniqueKey)
		if err != nil {
			return fmt.Errorf("load manager site identity: %w", err)
		}
		if len(uniqueResp.Kvs) > 0 && string(uniqueResp.Kvs[0].Value) != strconv.FormatInt(id, 10) {
			return fmt.Errorf("manager site for %s manager %d already exists", giteaURL, managerID)
		}
		compareUnique := clientv3.Compare(clientv3.ModRevision(uniqueKey), "=", 0)
		if len(uniqueResp.Kvs) > 0 {
			compareUnique = clientv3.Compare(clientv3.Value(uniqueKey), "=", strconv.FormatInt(id, 10))
		}
		ops := []clientv3.Op{
			clientv3.OpPut(siteKey, string(data)),
			clientv3.OpPut(uniqueKey, strconv.FormatInt(id, 10)),
		}
		ops = append(ops, extraOps...)
		if current.ID > 0 {
			oldUniqueKey := s.siteUniqueKey(current.GiteaURL, current.ManagerID)
			if oldUniqueKey != uniqueKey {
				ops = append(ops, clientv3.OpDelete(oldUniqueKey))
			}
		}
		resp, err := s.client.Txn(ctx).If(
			clientv3.Compare(clientv3.ModRevision(siteKey), "=", siteRevision),
			compareUnique,
		).Then(ops...).Commit()
		if err != nil {
			return fmt.Errorf("save manager site: %w", err)
		}
		if resp.Succeeded {
			return nil
		}
	}
}

func (s *etcdInfrastructureStore) DeleteSite(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("site id must be positive")
	}
	record, revision, err := s.loadSiteRecord(ctx, id)
	if err != nil {
		return err
	}
	if record.ID == 0 {
		return fmt.Errorf("manager site %d does not exist", id)
	}
	hasBindings, err := s.siteHasRuntimeBindings(ctx, id)
	if err != nil {
		return err
	}
	if hasBindings {
		return fmt.Errorf("manager site %d owns runtimes and cannot be deleted", id)
	}
	resp, err := s.client.Txn(ctx).If(
		clientv3.Compare(clientv3.ModRevision(s.siteKey(id)), "=", revision),
	).Then(
		clientv3.OpDelete(s.siteKey(id)),
		clientv3.OpDelete(s.siteUniqueKey(record.GiteaURL, record.ManagerID)),
	).Commit()
	if err != nil {
		return fmt.Errorf("delete manager site: %w", err)
	}
	if !resp.Succeeded {
		return fmt.Errorf("manager site %d changed while deleting", id)
	}
	return nil
}

func (s *etcdInfrastructureStore) siteHasRuntimeBindings(ctx context.Context, siteID int64) (bool, error) {
	response, err := s.client.Get(ctx, s.key("bindings/"), clientv3.WithPrefix())
	if err != nil {
		return false, fmt.Errorf("check manager site runtime bindings: %w", err)
	}
	for _, item := range response.Kvs {
		var binding RuntimeBinding
		if err := json.Unmarshal(item.Value, &binding); err != nil {
			return false, fmt.Errorf("decode runtime binding: %w", err)
		}
		if binding.SiteID == siteID {
			return true, nil
		}
	}
	return false, nil
}

func (s *etcdInfrastructureStore) SaveRuntimeBinding(ctx context.Context, binding RuntimeBinding) error {
	if err := validateRuntimeBinding(binding); err != nil {
		return err
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return fmt.Errorf("encode runtime binding: %w", err)
	}
	key := s.key("bindings/" + binding.RuntimeUUID)
	for {
		siteKey := s.siteKey(binding.SiteID)
		siteResponse, err := s.client.Get(ctx, siteKey)
		if err != nil {
			return fmt.Errorf("load runtime binding site: %w", err)
		}
		if len(siteResponse.Kvs) == 0 {
			return fmt.Errorf("manager site %d does not exist", binding.SiteID)
		}
		existing, err := s.client.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("load runtime binding: %w", err)
		}
		compare := clientv3.Compare(clientv3.Version(key), "=", 0)
		if len(existing.Kvs) > 0 {
			var current RuntimeBinding
			if err := json.Unmarshal(existing.Kvs[0].Value, &current); err != nil {
				return fmt.Errorf("decode runtime binding: %w", err)
			}
			if current.SiteID != binding.SiteID {
				return fmt.Errorf("runtime %s is owned by another manager site", binding.RuntimeUUID)
			}
			if string(existing.Kvs[0].Value) == string(data) {
				return nil
			}
			compare = clientv3.Compare(clientv3.ModRevision(key), "=", existing.Kvs[0].ModRevision)
		}
		response, err := s.client.Txn(ctx).If(
			s.executionComparisons(compare,
				clientv3.Compare(clientv3.ModRevision(siteKey), "=", siteResponse.Kvs[0].ModRevision))...,
		).Then(
			clientv3.OpPut(key, string(data)),
			clientv3.OpPut(siteKey, string(siteResponse.Kvs[0].Value)),
		).Commit()
		if err != nil {
			return fmt.Errorf("save runtime binding: %w", err)
		}
		if response.Succeeded {
			break
		}
		if s.leadership != nil {
			return errLeadershipLost
		}
	}
	return nil
}

func (s *etcdInfrastructureStore) DeleteRuntimeBinding(ctx context.Context, runtimeUUID string) error {
	if strings.TrimSpace(runtimeUUID) == "" {
		return fmt.Errorf("runtime uuid is required")
	}
	if response, err := s.client.Txn(ctx).If(s.executionComparisons()...).Then(
		clientv3.OpDelete(s.key("gateway-runtimes/"+runtimeUUID)),
		clientv3.OpDelete(s.key("bindings/"+runtimeUUID)),
	).Commit(); err != nil {
		return fmt.Errorf("delete runtime binding: %w", err)
	} else if !response.Succeeded {
		return errLeadershipLost
	}
	return nil
}

func (s *etcdInfrastructureStore) ListRuntimeBindings(ctx context.Context) ([]RuntimeBinding, error) {
	response, err := s.client.Get(ctx, s.key("bindings/"), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("list runtime bindings: %w", err)
	}
	bindings := make([]RuntimeBinding, 0, len(response.Kvs))
	for _, item := range response.Kvs {
		var binding RuntimeBinding
		if err := json.Unmarshal(item.Value, &binding); err != nil {
			return nil, fmt.Errorf("decode runtime binding: %w", err)
		}
		if err := validateRuntimeBinding(binding); err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].RuntimeUUID < bindings[j].RuntimeUUID })
	return bindings, nil
}

func (s *etcdInfrastructureStore) SaveGatewayRuntime(ctx context.Context, snapshot GatewayRuntimeSnapshot) error {
	if err := validateGatewayRuntimeSnapshot(snapshot); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode gateway runtime: %w", err)
	}
	key := s.key("gateway-runtimes/" + snapshot.RuntimeUUID)
	for {
		bindingKey := s.key("bindings/" + snapshot.RuntimeUUID)
		bindingResponse, err := s.client.Get(ctx, bindingKey)
		if err != nil {
			return fmt.Errorf("load gateway runtime binding: %w", err)
		}
		if len(bindingResponse.Kvs) == 0 {
			return fmt.Errorf("runtime %s has no runtime binding", snapshot.RuntimeUUID)
		}
		var binding RuntimeBinding
		if err := json.Unmarshal(bindingResponse.Kvs[0].Value, &binding); err != nil {
			return fmt.Errorf("decode gateway runtime binding: %w", err)
		}
		if binding.SiteID != snapshot.SiteID {
			return fmt.Errorf("runtime %s gateway route is owned by another manager site", snapshot.RuntimeUUID)
		}
		existing, err := s.client.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("load gateway runtime: %w", err)
		}
		compare := clientv3.Compare(clientv3.Version(key), "=", 0)
		if len(existing.Kvs) > 0 {
			var current GatewayRuntimeSnapshot
			if err := json.Unmarshal(existing.Kvs[0].Value, &current); err != nil {
				return fmt.Errorf("decode gateway runtime: %w", err)
			}
			if current.SiteID != snapshot.SiteID {
				return fmt.Errorf("runtime %s gateway route is owned by another manager site", snapshot.RuntimeUUID)
			}
			compare = clientv3.Compare(clientv3.ModRevision(key), "=", existing.Kvs[0].ModRevision)
		}
		write := clientv3.OpPut(key, string(data))
		if len(existing.Kvs) > 0 && string(existing.Kvs[0].Value) == string(data) {
			write = clientv3.OpGet(key)
		}
		response, err := s.client.Txn(ctx).If(
			s.executionComparisons(compare,
				clientv3.Compare(clientv3.ModRevision(bindingKey), "=", bindingResponse.Kvs[0].ModRevision))...,
		).Then(write).Commit()
		if err != nil {
			return fmt.Errorf("save gateway runtime: %w", err)
		}
		if response.Succeeded {
			break
		}
		if s.leadership != nil {
			return errLeadershipLost
		}
	}
	return nil
}

func (s *etcdInfrastructureStore) DeleteGatewayRuntime(ctx context.Context, runtimeUUID string) error {
	if strings.TrimSpace(runtimeUUID) == "" {
		return fmt.Errorf("runtime uuid is required")
	}
	if response, err := s.client.Txn(ctx).If(s.executionComparisons()...).Then(clientv3.OpDelete(s.key("gateway-runtimes/" + runtimeUUID))).Commit(); err != nil {
		return fmt.Errorf("delete gateway runtime: %w", err)
	} else if !response.Succeeded {
		return errLeadershipLost
	}
	return nil
}

func (s *etcdInfrastructureStore) ListGatewayRuntimes(ctx context.Context) ([]GatewayRuntimeSnapshot, error) {
	snapshots, _, err := s.listGatewayRuntimes(ctx)
	return snapshots, err
}

func (s *etcdInfrastructureStore) listGatewayRuntimes(ctx context.Context) ([]GatewayRuntimeSnapshot, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := s.client.Txn(ctx).Then(
		clientv3.OpGet(s.key("gateway-runtimes/"), clientv3.WithPrefix()),
		clientv3.OpGet(s.key("bindings/"), clientv3.WithPrefix()),
	).Commit()
	if err != nil {
		return nil, 0, fmt.Errorf("list gateway runtimes: %w", err)
	}
	bindings := make(map[string]int64)
	for _, item := range response.Responses[1].GetResponseRange().Kvs {
		var binding RuntimeBinding
		if err := json.Unmarshal(item.Value, &binding); err != nil {
			return nil, 0, fmt.Errorf("decode gateway binding: %w", err)
		}
		bindings[binding.RuntimeUUID] = binding.SiteID
	}
	items := response.Responses[0].GetResponseRange().Kvs
	snapshots := make([]GatewayRuntimeSnapshot, 0, len(items))
	for _, item := range items {
		var snapshot GatewayRuntimeSnapshot
		if err := json.Unmarshal(item.Value, &snapshot); err != nil {
			return nil, 0, fmt.Errorf("decode gateway runtime: %w", err)
		}
		if err := validateGatewayRuntimeSnapshot(snapshot); err != nil {
			return nil, 0, err
		}
		if bindings[snapshot.RuntimeUUID] != snapshot.SiteID {
			return nil, 0, fmt.Errorf("gateway runtime %s has no matching site binding", snapshot.RuntimeUUID)
		}
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].RuntimeUUID < snapshots[j].RuntimeUUID })
	return snapshots, response.Header.Revision, nil
}

func (s *etcdInfrastructureStore) loadEnabledSites(ctx context.Context) ([]ManagerSite, error) {
	records, err := s.listSiteRecords(ctx)
	if err != nil {
		return nil, err
	}
	sites := make([]ManagerSite, 0, len(records))
	for _, record := range records {
		if !record.Enabled {
			continue
		}
		secret, err := s.secret.decrypt(record.ManagerSecret)
		if err != nil {
			return nil, err
		}
		site := ManagerSite{
			ID:            record.ID,
			GiteaURL:      record.GiteaURL,
			ManagerID:     record.ManagerID,
			ManagerSecret: secret,
		}
		if _, err := normalizeGiteaURL(site.GiteaURL); err != nil {
			return nil, fmt.Errorf("validate manager site %d: %w", site.ID, err)
		}
		if site.ManagerID <= 0 || strings.TrimSpace(site.ManagerSecret) == "" {
			return nil, fmt.Errorf("validate manager site %d: manager identity is incomplete", site.ID)
		}
		sites = append(sites, site)
	}
	if len(sites) == 0 {
		return nil, errInfrastructureStateEmpty
	}
	return sites, nil
}

func (s *etcdInfrastructureStore) listSiteRecords(ctx context.Context) ([]etcdSiteRecord, error) {
	resp, err := s.client.Get(ctx, s.key("sites/"), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("list manager sites: %w", err)
	}
	records := make([]etcdSiteRecord, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var record etcdSiteRecord
		if err := json.Unmarshal(kv.Value, &record); err != nil {
			return nil, fmt.Errorf("decode manager site: %w", err)
		}
		if record.ID <= 0 {
			return nil, fmt.Errorf("decode manager site: id is missing")
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].ID < records[j].ID
	})
	return records, nil
}

func (s *etcdInfrastructureStore) loadSiteRecord(ctx context.Context, id int64) (etcdSiteRecord, int64, error) {
	resp, err := s.client.Get(ctx, s.siteKey(id))
	if err != nil {
		return etcdSiteRecord{}, 0, fmt.Errorf("load manager site: %w", err)
	}
	if len(resp.Kvs) == 0 {
		return etcdSiteRecord{}, 0, nil
	}
	var record etcdSiteRecord
	if err := json.Unmarshal(resp.Kvs[0].Value, &record); err != nil {
		return etcdSiteRecord{}, 0, fmt.Errorf("decode manager site: %w", err)
	}
	return record, resp.Kvs[0].ModRevision, nil
}

func (s *etcdInfrastructureStore) nextSiteID(ctx context.Context) (int64, error) {
	for {
		value, revision, err := s.loadSiteSequence(ctx)
		if err != nil {
			return 0, err
		}
		next := value + 1
		resp, err := s.client.Txn(ctx).If(
			clientv3.Compare(clientv3.ModRevision(s.key("site-sequence")), "=", revision),
		).Then(
			clientv3.OpPut(s.key("site-sequence"), strconv.FormatInt(next, 10)),
		).Commit()
		if err != nil {
			return 0, fmt.Errorf("advance manager site sequence: %w", err)
		}
		if resp.Succeeded {
			return next, nil
		}
	}
}

func (s *etcdInfrastructureStore) ensureSiteSequenceAtLeast(ctx context.Context, id int64) error {
	for {
		value, revision, err := s.loadSiteSequence(ctx)
		if err != nil {
			return err
		}
		if value >= id {
			return nil
		}
		resp, err := s.client.Txn(ctx).If(
			clientv3.Compare(clientv3.ModRevision(s.key("site-sequence")), "=", revision),
		).Then(
			clientv3.OpPut(s.key("site-sequence"), strconv.FormatInt(id, 10)),
		).Commit()
		if err != nil {
			return fmt.Errorf("advance manager site sequence: %w", err)
		}
		if resp.Succeeded {
			return nil
		}
	}
}

func (s *etcdInfrastructureStore) loadSiteSequence(ctx context.Context) (int64, int64, error) {
	resp, err := s.client.Get(ctx, s.key("site-sequence"))
	if err != nil {
		return 0, 0, fmt.Errorf("load manager site sequence: %w", err)
	}
	if len(resp.Kvs) == 0 {
		return 0, 0, nil
	}
	value, err := strconv.ParseInt(string(resp.Kvs[0].Value), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("decode manager site sequence: %w", err)
	}
	return value, resp.Kvs[0].ModRevision, nil
}

func (s *etcdInfrastructureStore) key(name string) string {
	return s.prefix + "/" + strings.TrimLeft(name, "/")
}

func (s *etcdInfrastructureStore) siteKey(id int64) string {
	return s.key("sites/" + fmt.Sprintf("%020d", id))
}

func (s *etcdInfrastructureStore) siteUniqueKey(giteaURL string, managerID int64) string {
	identity := fmt.Sprintf("%s\x00%d", giteaURL, managerID)
	return s.key("site-identities/" + base64.RawURLEncoding.EncodeToString([]byte(identity)))
}

var _ managerInfrastructureStore = (*etcdInfrastructureStore)(nil)

// WatchGatewayRuntimes rebuilds one revision-consistent view after relevant changes.
func (s *etcdInfrastructureStore) WatchGatewayRuntimes(ctx context.Context, routes *gatewayRouteStore) {
	for ctx.Err() == nil {
		snapshots, revision, err := s.listGatewayRuntimes(ctx)
		if err == nil {
			err = routes.ReplaceSharedGatewayRuntimes(snapshots)
		}
		if err == nil {
			watchCtx, cancel := context.WithCancel(ctx)
			changes := s.client.Watch(watchCtx, s.key(""), clientv3.WithPrefix(), clientv3.WithRev(revision+1))
			for response := range changes {
				if response.Err() != nil {
					err = response.Err()
					break
				}
				relevant := false
				for _, event := range response.Events {
					key := string(event.Kv.Key)
					if strings.HasPrefix(key, s.key("gateway-runtimes/")) || strings.HasPrefix(key, s.key("bindings/")) {
						relevant = true
						break
					}
				}
				if !relevant {
					continue
				}
				snapshots, _, err = s.listGatewayRuntimes(ctx)
				if err == nil {
					err = routes.ReplaceSharedGatewayRuntimes(snapshots)
				}
				if err != nil {
					break
				}
			}
			cancel()
		}
		if ctx.Err() != nil {
			return
		}
		// A failed shared-state read must not leave stale routes usable indefinitely.
		_ = routes.ReplaceSharedGatewayRuntimes(nil)
		log.Printf("watch shared gateway runtimes: %v; rebuilding snapshot", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *etcdInfrastructureStore) executionComparisons(comparisons ...clientv3.Cmp) []clientv3.Cmp {
	if s.leadership != nil {
		return append(s.leadership.comparisons(), comparisons...)
	}
	return comparisons
}
