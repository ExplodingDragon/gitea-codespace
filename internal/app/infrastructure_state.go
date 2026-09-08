// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	managerStateDriverEnv        = "GITEA_CODESPACE_STATE"
	managerStatePathEnv          = "GITEA_CODESPACE_STATE_PATH"
	managerStateEncryptionKeyEnv = "GITEA_CODESPACE_STATE_ENCRYPTION_KEY"
	managerStateEtcdEndpointsEnv = "GITEA_CODESPACE_ETCD_ENDPOINTS"
	managerStateEtcdPrefixEnv    = "GITEA_CODESPACE_ETCD_PREFIX"
	managerNodeIDEnv             = "GITEA_CODESPACE_NODE_ID"
	managerAdminListenEnv        = "GITEA_CODESPACE_ADMIN_LISTEN"
	managerAdminTokenEnv         = "GITEA_CODESPACE_ADMIN_TOKEN"
)

var errInfrastructureStateEmpty = errors.New("manager infrastructure state is empty")

type managerInfrastructureStore interface {
	Close() error
	LoadRuntimeConfig(context.Context) (InfrastructureRuntimeConfig, error)
	SaveConfigOnly(context.Context, Config) error
	LoadConfigOnly(context.Context) (Config, error)
	ListSites(context.Context) ([]AdminSite, error)
	LoadSite(context.Context, int64) (ManagerSite, error)
	UpsertSite(context.Context, UpsertAdminSiteOptions) (int64, error)
	DeleteSite(context.Context, int64) error
	SaveRuntimeBinding(context.Context, RuntimeBinding) error
	DeleteRuntimeBinding(context.Context, string) error
	ListRuntimeBindings(context.Context) ([]RuntimeBinding, error)
	SaveGatewayRuntime(context.Context, GatewayRuntimeSnapshot) error
	DeleteGatewayRuntime(context.Context, string) error
	ListGatewayRuntimes(context.Context) ([]GatewayRuntimeSnapshot, error)
	LoadGatewaySSHHostKey(context.Context) (gatewaySSHHostKey, error)
	WatchGatewayRuntimes(context.Context, *gatewayRouteStore)
}

// InfrastructureRuntimeConfig is the active local view used to start one Manager process.
type InfrastructureRuntimeConfig struct {
	Config Config
	Sites  []ManagerSite
	NodeID string
	store  *etcdInfrastructureStore
}

// ManagerSite is one enabled Gitea control-plane identity used by the running process.
type ManagerSite struct {
	ID                  int64
	GiteaURL            string
	ManagerID           int64
	ManagerSecret       string
	InventoryGeneration int64
}

// RuntimeBinding records the Manager-owned placement of one runtime.
type RuntimeBinding struct {
	RuntimeUUID       string
	SiteID            int64
	BackendID         string
	CodespaceID       int64
	OperationRVersion int64
	EnvironmentTag    string
}

// GatewayRuntimeSnapshot contains only the backend coordinates needed to reach one runtime.
type GatewayRuntimeSnapshot struct {
	RuntimeUUID      string                    `json:"runtime_uuid"`
	SiteID           int64                     `json:"site_id"`
	InstanceName     string                    `json:"instance_name"`
	Workdir          string                    `json:"workdir"`
	UID              uint32                    `json:"uid"`
	GID              uint32                    `json:"gid"`
	ContainerID      string                    `json:"container_id"`
	ContainerUser    string                    `json:"container_user"`
	ContainerWorkdir string                    `json:"container_workdir"`
	EditorPort       uint32                    `json:"editor_port"`
	Endpoints        []GatewayEndpointSnapshot `json:"endpoints"`
}

// GatewayEndpointSnapshot contains one HTTP endpoint route without authorization data.
type GatewayEndpointSnapshot struct {
	EndpointID   string `json:"endpoint_id"`
	Label        string `json:"label"`
	UpstreamPort uint32 `json:"upstream_port"`
	Public       bool   `json:"public"`
}

// AdminSite is the public local-admin view of one Gitea site.
type AdminSite struct {
	ID        int64  `json:"id"`
	GiteaURL  string `json:"gitea_url"`
	ManagerID int64  `json:"manager_id"`
	Enabled   bool   `json:"enabled"`
}

// UpsertAdminSiteOptions stores one Gitea site identity in Manager state.
type UpsertAdminSiteOptions struct {
	ID            int64  `json:"id"`
	GiteaURL      string `json:"gitea_url"`
	ManagerID     int64  `json:"manager_id"`
	ManagerSecret string `json:"manager_secret"`
	Enabled       bool   `json:"enabled"`
}

type encryptedValue struct {
	Nonce string `json:"nonce"`
	Data  string `json:"data"`
}

type managerSecretCodec struct {
	aad []byte
	gcm cipher.AEAD
}

func openInfrastructureStore(driver string) (*etcdInfrastructureStore, error) {
	switch driver {
	case "", "embedded":
		return openEmbeddedInfrastructureStore()
	case "etcd":
		return openEtcdInfrastructureStore()
	default:
		return nil, fmt.Errorf("manager state driver %q is not supported", driver)
	}
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

func validateRuntimeBinding(binding RuntimeBinding) error {
	if strings.TrimSpace(binding.RuntimeUUID) == "" || binding.SiteID <= 0 || strings.TrimSpace(binding.BackendID) == "" ||
		binding.CodespaceID <= 0 || binding.OperationRVersion <= 0 {
		return fmt.Errorf("runtime binding is incomplete")
	}
	return nil
}

func validateGatewayRuntimeSnapshot(snapshot GatewayRuntimeSnapshot) error {
	if strings.TrimSpace(snapshot.RuntimeUUID) == "" || snapshot.SiteID <= 0 || strings.TrimSpace(snapshot.InstanceName) == "" ||
		strings.TrimSpace(snapshot.Workdir) == "" || strings.TrimSpace(snapshot.ContainerID) == "" ||
		strings.TrimSpace(snapshot.ContainerUser) == "" || strings.TrimSpace(snapshot.ContainerWorkdir) == "" || snapshot.EditorPort == 0 {
		return fmt.Errorf("gateway runtime snapshot is incomplete")
	}
	seen := make(map[string]struct{}, len(snapshot.Endpoints))
	for _, endpoint := range snapshot.Endpoints {
		if strings.TrimSpace(endpoint.EndpointID) == "" || endpoint.UpstreamPort == 0 {
			return fmt.Errorf("gateway endpoint snapshot is incomplete")
		}
		if _, ok := seen[endpoint.EndpointID]; ok {
			return fmt.Errorf("gateway endpoint %q is duplicated", endpoint.EndpointID)
		}
		seen[endpoint.EndpointID] = struct{}{}
	}
	return nil
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
	if len(nonce) != c.gcm.NonceSize() {
		return "", fmt.Errorf("encrypted manager state nonce has invalid length")
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
		return nil, fmt.Errorf("%s is required", managerStateEncryptionKeyEnv)
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
