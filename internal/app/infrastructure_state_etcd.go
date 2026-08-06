// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

const defaultManagerStateEtcdPrefix = "/gitea-codespace"

type etcdInfrastructureStore struct {
	client *clientv3.Client
	prefix string
	secret managerSecretCodec
}

type etcdSiteRecord struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
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
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
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
		return nil, fmt.Errorf("%s is required when %s=etcd", managerStateEtcdEndpointsEnv, managerStateDriverEnv)
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
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *etcdInfrastructureStore) LoadRuntimeConfig(ctx context.Context) (InfrastructureRuntimeConfig, error) {
	config, err := s.LoadConfigOnly(ctx)
	if err != nil {
		return InfrastructureRuntimeConfig{}, err
	}
	site, err := s.loadActiveSite(ctx)
	if err != nil {
		return InfrastructureRuntimeConfig{}, err
	}
	inventoryGeneration, err := loadInventoryGeneration(config.Node.StateDir)
	if err != nil {
		return InfrastructureRuntimeConfig{}, err
	}
	return InfrastructureRuntimeConfig{
		Config: config,
		ManagerState: ManagerState{
			GiteaURL:            site.GiteaURL,
			ManagerID:           site.ManagerID,
			ManagerSecret:       site.ManagerSecret,
			InventoryGeneration: inventoryGeneration,
		},
		NodeID:      managerNodeID(),
		NodeRole:    managerNodeRole(),
		AdminListen: strings.TrimSpace(os.Getenv(managerAdminListenEnv)),
		AdminToken:  strings.TrimSpace(os.Getenv(managerAdminTokenEnv)),
	}, nil
}

func (s *etcdInfrastructureStore) SaveRuntimeConfig(ctx context.Context, config Config, managerState ManagerState) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if err := managerState.Validate(); err != nil {
		return err
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode manager runtime config: %w", err)
	}
	secret, err := s.secret.encrypt(managerState.ManagerSecret)
	if err != nil {
		return err
	}
	giteaURL, err := normalizeGiteaURL(managerState.GiteaURL)
	if err != nil {
		return err
	}
	if err := s.ensureSiteSequenceAtLeast(ctx, 1); err != nil {
		return err
	}
	site := etcdSiteRecord{
		ID:            1,
		Name:          "default",
		GiteaURL:      giteaURL,
		ManagerID:     managerState.ManagerID,
		ManagerSecret: secret,
		Enabled:       true,
	}
	if err := s.saveSiteRecord(ctx, site, clientv3.OpPut(s.key("config"), string(configJSON))); err != nil {
		return fmt.Errorf("save manager runtime state: %w", err)
	}
	return nil
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
			Name:      record.Name,
			GiteaURL:  record.GiteaURL,
			ManagerID: record.ManagerID,
			Enabled:   record.Enabled,
		})
	}
	return sites, nil
}

func (s *etcdInfrastructureStore) UpsertSite(ctx context.Context, opts UpsertAdminSiteOptions) (int64, error) {
	giteaURL, err := normalizeGiteaURL(opts.GiteaURL)
	if err != nil {
		return 0, err
	}
	if opts.ManagerID <= 0 {
		return 0, fmt.Errorf("manager_id must be a positive integer")
	}
	if strings.TrimSpace(opts.ManagerSecret) == "" {
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
	secret, err := s.secret.encrypt(strings.TrimSpace(opts.ManagerSecret))
	if err != nil {
		return 0, err
	}
	record := etcdSiteRecord{
		ID:            id,
		Name:          strings.TrimSpace(opts.Name),
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

func (s *etcdInfrastructureStore) loadActiveSite(ctx context.Context) (infrastructureSite, error) {
	records, err := s.listSiteRecords(ctx)
	if err != nil {
		return infrastructureSite{}, err
	}
	for _, record := range records {
		if !record.Enabled {
			continue
		}
		secret, err := s.secret.decrypt(record.ManagerSecret)
		if err != nil {
			return infrastructureSite{}, err
		}
		site := infrastructureSite{
			ID:            record.ID,
			Name:          record.Name,
			GiteaURL:      record.GiteaURL,
			ManagerID:     record.ManagerID,
			ManagerSecret: secret,
			Enabled:       record.Enabled,
		}
		if _, err := normalizeGiteaURL(site.GiteaURL); err != nil {
			return infrastructureSite{}, fmt.Errorf("validate manager site: %w", err)
		}
		if site.ManagerID <= 0 || strings.TrimSpace(site.ManagerSecret) == "" {
			return infrastructureSite{}, fmt.Errorf("validate manager site: manager identity is incomplete")
		}
		return site, nil
	}
	return infrastructureSite{}, errInfrastructureStateEmpty
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
