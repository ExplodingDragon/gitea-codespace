// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	managerStateDriverEnv        = "GITEA_CODESPACE_STATE"
	managerStatePathEnv          = "GITEA_CODESPACE_STATE_PATH"
	managerStateEncryptionKeyEnv = "GITEA_CODESPACE_STATE_ENCRYPTION_KEY"
	managerStateEtcdEndpointsEnv = "GITEA_CODESPACE_ETCD_ENDPOINTS"
	managerStateEtcdPrefixEnv    = "GITEA_CODESPACE_ETCD_PREFIX"
	managerNodeIDEnv             = "GITEA_CODESPACE_NODE_ID"
	managerNodeRoleEnv           = "GITEA_CODESPACE_NODE_ROLE"
	managerAdminListenEnv        = "GITEA_CODESPACE_ADMIN_LISTEN"
	managerAdminTokenEnv         = "GITEA_CODESPACE_ADMIN_TOKEN"
)

var errInfrastructureStateEmpty = errors.New("manager infrastructure state is empty")

const (
	managerNodeRoleAll     = "all"
	managerNodeRoleGateway = "gateway"
)

type managerInfrastructureStore interface {
	Close() error
	LoadRuntimeConfig(context.Context) (InfrastructureRuntimeConfig, error)
	SaveRuntimeConfig(context.Context, Config, ManagerState) error
	SaveConfigOnly(context.Context, Config) error
	LoadConfigOnly(context.Context) (Config, error)
	ListSites(context.Context) ([]AdminSite, error)
	UpsertSite(context.Context, UpsertAdminSiteOptions) (int64, error)
	DeleteSite(context.Context, int64) error
}

// InfrastructureRuntimeConfig is the active local view used to start one Manager process.
type InfrastructureRuntimeConfig struct {
	Config       Config
	ManagerState ManagerState
	NodeID       string
	NodeRole     string
	AdminListen  string
	AdminToken   string
}

// AdminSite is the public local-admin view of one Gitea site.
type AdminSite struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	GiteaURL  string `json:"gitea_url"`
	ManagerID int64  `json:"manager_id"`
	Enabled   bool   `json:"enabled"`
}

// UpsertAdminSiteOptions stores one Gitea site identity in Manager state.
type UpsertAdminSiteOptions struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	GiteaURL      string `json:"gitea_url"`
	ManagerID     int64  `json:"manager_id"`
	ManagerSecret string `json:"manager_secret"`
	Enabled       bool   `json:"enabled"`
}

type infrastructureSite struct {
	ID            int64
	Name          string
	GiteaURL      string
	ManagerID     int64
	ManagerSecret string
	Enabled       bool
}

type encryptedValue struct {
	Nonce string `json:"nonce"`
	Data  string `json:"data"`
}

type infrastructureStore struct {
	db     *sql.DB
	secret managerSecretCodec
}

type managerSecretCodec struct {
	aad []byte
	gcm cipher.AEAD
}

// LoadInfrastructureRuntimeConfig loads Manager business configuration from the configured state store.
func LoadInfrastructureRuntimeConfig(configPath string) (InfrastructureRuntimeConfig, bool, error) {
	driver := strings.ToLower(strings.TrimSpace(os.Getenv(managerStateDriverEnv)))
	if driver == "" {
		return InfrastructureRuntimeConfig{}, false, nil
	}
	store, err := openInfrastructureStore(driver)
	if err != nil {
		return InfrastructureRuntimeConfig{}, true, err
	}
	defer func() { _ = store.Close() }()

	runtimeConfig, err := store.LoadRuntimeConfig(context.Background())
	if err == nil {
		return runtimeConfig, true, nil
	}
	if !errors.Is(err, errInfrastructureStateEmpty) || strings.TrimSpace(configPath) == "" {
		return InfrastructureRuntimeConfig{}, true, err
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		return InfrastructureRuntimeConfig{}, true, fmt.Errorf("import config into manager state: %w", err)
	}
	if err := store.SaveConfigOnly(context.Background(), config); err != nil {
		return InfrastructureRuntimeConfig{}, true, err
	}
	runtimeConfig, err = store.LoadRuntimeConfig(context.Background())
	return runtimeConfig, true, err
}

func openInfrastructureStore(driver string) (managerInfrastructureStore, error) {
	switch driver {
	case "local":
		return openLocalInfrastructureStore()
	case "etcd":
		return openEtcdInfrastructureStore()
	default:
		return nil, fmt.Errorf("manager state driver %q is not supported by this binary", driver)
	}
}

func openLocalInfrastructureStore() (*infrastructureStore, error) {
	codec, err := newManagerSecretCodec()
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(os.Getenv(managerStatePathEnv))
	if path == "" {
		path = filepath.Join("codespace-state", "manager.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create manager state directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open manager state database: %w", err)
	}
	store := &infrastructureStore{db: db, secret: codec}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func newManagerSecretCodec() (managerSecretCodec, error) {
	key, err := managerStateEncryptionKey()
	if err != nil {
		return managerSecretCodec{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return managerSecretCodec{}, fmt.Errorf("create manager state cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return managerSecretCodec{}, fmt.Errorf("create manager state AEAD: %w", err)
	}
	return managerSecretCodec{aad: []byte("gitea-codespace-manager-state-v1"), gcm: gcm}, nil
}

func (s *infrastructureStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *infrastructureStore) init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS manager_site (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			gitea_url TEXT NOT NULL,
			manager_id INTEGER NOT NULL,
			manager_secret TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			UNIQUE(gitea_url, manager_id)
		)`,
		`CREATE TABLE IF NOT EXISTS manager_runtime_config (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			config_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS manager_runtime_binding (
			runtime_uuid TEXT PRIMARY KEY,
			site_id INTEGER NOT NULL,
			backend_id TEXT NOT NULL,
			codespace_id INTEGER NOT NULL DEFAULT 0,
			operation_rversion INTEGER NOT NULL DEFAULT 0,
			environment_tag TEXT NOT NULL DEFAULT ''
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize manager state database: %w", err)
		}
	}
	return nil
}

func (s *infrastructureStore) LoadRuntimeConfig(ctx context.Context) (InfrastructureRuntimeConfig, error) {
	var configJSON string
	err := s.db.QueryRowContext(ctx, `SELECT config_json FROM manager_runtime_config WHERE id = 1`).Scan(&configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return InfrastructureRuntimeConfig{}, errInfrastructureStateEmpty
	}
	if err != nil {
		return InfrastructureRuntimeConfig{}, fmt.Errorf("load manager runtime config: %w", err)
	}
	config := DefaultConfig()
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return InfrastructureRuntimeConfig{}, fmt.Errorf("decode manager runtime config: %w", err)
	}
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return InfrastructureRuntimeConfig{}, fmt.Errorf("validate manager runtime config from state: %w", err)
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

func (s *infrastructureStore) SaveRuntimeConfig(ctx context.Context, config Config, managerState ManagerState) error {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO manager_runtime_config(id, config_json) VALUES(1, ?)
		ON CONFLICT(id) DO UPDATE SET config_json = excluded.config_json`, string(configJSON)); err != nil {
		return fmt.Errorf("save manager runtime config: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO manager_site(id, name, gitea_url, manager_id, manager_secret, enabled)
		VALUES(1, 'default', ?, ?, ?, 1)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, gitea_url = excluded.gitea_url,
			manager_id = excluded.manager_id, manager_secret = excluded.manager_secret, enabled = 1`,
		managerState.GiteaURL, managerState.ManagerID, secret); err != nil {
		return fmt.Errorf("save manager site: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit manager state: %w", err)
	}
	return nil
}

func (s *infrastructureStore) SaveConfigOnly(ctx context.Context, config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode manager runtime config: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO manager_runtime_config(id, config_json) VALUES(1, ?)
		ON CONFLICT(id) DO UPDATE SET config_json = excluded.config_json`, string(configJSON)); err != nil {
		return fmt.Errorf("save manager runtime config: %w", err)
	}
	return nil
}

func (s *infrastructureStore) LoadConfigOnly(ctx context.Context) (Config, error) {
	var configJSON string
	err := s.db.QueryRowContext(ctx, `SELECT config_json FROM manager_runtime_config WHERE id = 1`).Scan(&configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Config{}, errInfrastructureStateEmpty
	}
	if err != nil {
		return Config{}, fmt.Errorf("load manager runtime config: %w", err)
	}
	config := DefaultConfig()
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return Config{}, fmt.Errorf("decode manager runtime config: %w", err)
	}
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate manager runtime config from state: %w", err)
	}
	return config, nil
}

func (s *infrastructureStore) ListSites(ctx context.Context) ([]AdminSite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, gitea_url, manager_id, enabled FROM manager_site ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list manager sites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var sites []AdminSite
	for rows.Next() {
		var site AdminSite
		var enabled int
		if err := rows.Scan(&site.ID, &site.Name, &site.GiteaURL, &site.ManagerID, &enabled); err != nil {
			return nil, fmt.Errorf("scan manager site: %w", err)
		}
		site.Enabled = enabled == 1
		sites = append(sites, site)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list manager sites: %w", err)
	}
	return sites, nil
}

func (s *infrastructureStore) UpsertSite(ctx context.Context, opts UpsertAdminSiteOptions) (int64, error) {
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
	secret, err := s.secret.encrypt(strings.TrimSpace(opts.ManagerSecret))
	if err != nil {
		return 0, err
	}
	name := strings.TrimSpace(opts.Name)
	enabled := 0
	if opts.Enabled {
		enabled = 1
	}
	if opts.ID <= 0 {
		result, err := s.db.ExecContext(ctx, `INSERT INTO manager_site(name, gitea_url, manager_id, manager_secret, enabled)
			VALUES(?, ?, ?, ?, ?)`, name, giteaURL, opts.ManagerID, secret, enabled)
		if err != nil {
			return 0, fmt.Errorf("create manager site: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("read manager site id: %w", err)
		}
		return id, nil
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO manager_site(id, name, gitea_url, manager_id, manager_secret, enabled)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, gitea_url = excluded.gitea_url,
			manager_id = excluded.manager_id, manager_secret = excluded.manager_secret, enabled = excluded.enabled`,
		opts.ID, name, giteaURL, opts.ManagerID, secret, enabled); err != nil {
		return 0, fmt.Errorf("save manager site: %w", err)
	}
	return opts.ID, nil
}

func (s *infrastructureStore) DeleteSite(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("site id must be positive")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM manager_site WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete manager site: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read deleted manager site count: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("manager site %d does not exist", id)
	}
	return nil
}

func (s *infrastructureStore) loadActiveSite(ctx context.Context) (infrastructureSite, error) {
	var site infrastructureSite
	var encryptedSecret string
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT id, name, gitea_url, manager_id, manager_secret, enabled
		FROM manager_site WHERE enabled = 1 ORDER BY id LIMIT 1`).Scan(&site.ID, &site.Name, &site.GiteaURL, &site.ManagerID, &encryptedSecret, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return infrastructureSite{}, errInfrastructureStateEmpty
	}
	if err != nil {
		return infrastructureSite{}, fmt.Errorf("load manager site: %w", err)
	}
	secret, err := s.secret.decrypt(encryptedSecret)
	if err != nil {
		return infrastructureSite{}, err
	}
	site.ManagerSecret = secret
	site.Enabled = enabled == 1
	if _, err := normalizeGiteaURL(site.GiteaURL); err != nil {
		return infrastructureSite{}, fmt.Errorf("validate manager site: %w", err)
	}
	if site.ManagerID <= 0 || strings.TrimSpace(site.ManagerSecret) == "" {
		return infrastructureSite{}, fmt.Errorf("validate manager site: manager identity is incomplete")
	}
	return site, nil
}

func (c managerSecretCodec) encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("create manager state nonce: %w", err)
	}
	sealed := c.gcm.Seal(nil, nonce, []byte(plaintext), c.aad)
	encoded, err := json.Marshal(encryptedValue{
		Nonce: base64.RawStdEncoding.EncodeToString(nonce),
		Data:  base64.RawStdEncoding.EncodeToString(sealed),
	})
	if err != nil {
		return "", fmt.Errorf("encode encrypted manager state value: %w", err)
	}
	return string(encoded), nil
}

func (c managerSecretCodec) decrypt(value string) (string, error) {
	var encrypted encryptedValue
	if err := json.Unmarshal([]byte(value), &encrypted); err != nil {
		return "", fmt.Errorf("decode encrypted manager state value: %w", err)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(encrypted.Nonce)
	if err != nil {
		return "", fmt.Errorf("decode encrypted manager state nonce: %w", err)
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(encrypted.Data)
	if err != nil {
		return "", fmt.Errorf("decode encrypted manager state data: %w", err)
	}
	plaintext, err := c.gcm.Open(nil, nonce, ciphertext, c.aad)
	if err != nil {
		return "", fmt.Errorf("decrypt manager state value: %w", err)
	}
	return string(plaintext), nil
}

func managerStateEncryptionKey() ([]byte, error) {
	value := strings.TrimSpace(os.Getenv(managerStateEncryptionKeyEnv))
	if value == "" {
		return nil, fmt.Errorf("%s is required when %s is set", managerStateEncryptionKeyEnv, managerStateDriverEnv)
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be a base64 encoded 32-byte key", managerStateEncryptionKeyEnv)
	}
	return key, nil
}

func managerNodeID() string {
	nodeID := strings.TrimSpace(os.Getenv(managerNodeIDEnv))
	if nodeID != "" {
		return nodeID
	}
	host, err := os.Hostname()
	if err == nil && strings.TrimSpace(host) != "" {
		return strings.TrimSpace(host)
	}
	return "local"
}

func managerNodeRole() string {
	role := strings.ToLower(strings.TrimSpace(os.Getenv(managerNodeRoleEnv)))
	if role == "" {
		return managerNodeRoleAll
	}
	return role
}

func validateManagerNodeRole(role string) error {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "", managerNodeRoleAll, managerNodeRoleGateway:
		return nil
	default:
		return fmt.Errorf("%s must be all or gateway", managerNodeRoleEnv)
	}
}

func validateAdminListen(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := url.Parse("http://" + value)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%s must be a host:port listener address", managerAdminListenEnv)
	}
	return nil
}

// RunAdmin starts the local Manager administration API.
func RunAdmin(output io.Writer) error {
	if output == nil {
		return fmt.Errorf("output is nil")
	}
	listen := strings.TrimSpace(os.Getenv(managerAdminListenEnv))
	if listen == "" {
		listen = "127.0.0.1:18080"
	}
	if err := validateAdminListen(listen); err != nil {
		return err
	}
	token := strings.TrimSpace(os.Getenv(managerAdminTokenEnv))
	if token == "" {
		return fmt.Errorf("%s is required", managerAdminTokenEnv)
	}
	driver := strings.ToLower(strings.TrimSpace(os.Getenv(managerStateDriverEnv)))
	if driver == "" {
		return fmt.Errorf("%s must be set to local or etcd", managerStateDriverEnv)
	}
	store, err := openInfrastructureStore(driver)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	server := &http.Server{
		Addr:              listen,
		Handler:           newInfrastructureAdminHandler(store, token),
		ReadHeaderTimeout: gatewayHTTPReadHeaderTime,
		MaxHeaderBytes:    gatewayHTTPMaxHeaderBytes,
	}
	_, _ = fmt.Fprintf(output, "codespace manager admin listening on %s\n", listen)
	return server.ListenAndServe()
}

func newInfrastructureAdminHandler(store managerInfrastructureStore, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "pass"})
	})
	mux.HandleFunc("/api/sites", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdmin(writer, request, token) {
			return
		}
		switch request.Method {
		case http.MethodGet:
			sites, err := store.ListSites(request.Context())
			if err != nil {
				writeAdminError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"sites": sites})
		case http.MethodPost:
			var opts UpsertAdminSiteOptions
			if err := json.NewDecoder(request.Body).Decode(&opts); err != nil {
				writeAdminError(writer, fmt.Errorf("decode site request: %w", err))
				return
			}
			id, err := store.UpsertSite(request.Context(), opts)
			if err != nil {
				writeAdminError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"id": id})
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/sites/", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdmin(writer, request, token) {
			return
		}
		if request.Method != http.MethodDelete {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(request.URL.Path, "/api/sites/"), 10, 64)
		if err != nil {
			writeAdminError(writer, fmt.Errorf("site id must be a positive integer"))
			return
		}
		if err := store.DeleteSite(request.Context(), id); err != nil {
			writeAdminError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"status": "deleted"})
	})
	mux.HandleFunc("/api/config", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdmin(writer, request, token) {
			return
		}
		switch request.Method {
		case http.MethodGet:
			config, err := store.LoadConfigOnly(request.Context())
			if err != nil {
				writeAdminError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, config)
		case http.MethodPut:
			config := DefaultConfig()
			if err := json.NewDecoder(request.Body).Decode(&config); err != nil {
				writeAdminError(writer, fmt.Errorf("decode config request: %w", err))
				return
			}
			config.applyDefaults()
			if err := store.SaveConfigOnly(request.Context(), config); err != nil {
				writeAdminError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"status": "saved"})
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return loggingMiddleware(mux)
}

func authorizeInfrastructureAdmin(writer http.ResponseWriter, request *http.Request, token string) bool {
	if request.Header.Get("Authorization") != "Bearer "+token {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": "admin token is required"})
		return false
	}
	return true
}

func writeAdminError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errInfrastructureStateEmpty) {
		status = http.StatusNotFound
	}
	writeJSON(writer, status, map[string]any{"error": err.Error()})
}
