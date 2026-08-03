// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigYAML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "codespace.yaml")
	content := `
node:
  state_dir: "state"
  name: "yaml-manager"
  poll_interval: "1s"
  capacity_total: 3
  startup_workers: 2
  cleanup_workers: 5

gateway:
  http:
    listen: ":19091"
    public_url: "https://codespace.example.com"
  ssh:
    listen: ":19022"
    public_addr: "codespace.example.com:2222"
    handshake_timeout: "20s"
    max_channels_per_connection: 7
    auth:
      max_attempts_per_ip_per_minute: 11
      max_attempts_per_codespace_per_minute: 12
      max_attempts_per_ip_codespace_per_minute: 13
      max_attempts_per_public_key_per_minute: 14
      failure_window: "5m"
  sessions:
    ttl: "4h"
    idle_timeout: "2m"
    revalidate_interval: "45s"
    max_per_codespace: 9
    max_per_user: 99

runtime:
  incus:
    endpoint: "unix:///var/lib/incus/unix.socket"
    project:
      name: "gitea-codespace"
      manage: true
    storage:
      pool: "default"
    network:
      name: "csnet"
      manage: true
  environments:
    - tag: "Default"
      description: " General development "
      type: "vm"
      source:
        image: "images:debian/12"
      resources:
        cpu: 2
        memory: "1GiB"
        root_disk: "10GiB"
      profiles: ["default"]
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml config: %v", err)
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}

	if config.Node.StateDir != filepath.Join(dir, "state") {
		t.Fatalf("manager state dir = %q", config.Node.StateDir)
	}
	if config.Node.Name != "yaml-manager" || config.Gateway.HTTP.PublicURL != "https://codespace.example.com" {
		t.Fatalf("config = %#v", config)
	}
	if config.Gateway.SSH.PublicAddr != "codespace.example.com:2222" {
		t.Fatalf("manager gateway ssh addr = %q", config.Gateway.SSH.PublicAddr)
	}
	if config.Gateway.HTTP.Listen != ":19091" || config.Gateway.SSH.Listen != ":19022" {
		t.Fatalf("gateway listeners = %#v", config.Gateway)
	}
	if config.Gateway.Sessions.RevalidateInterval.ToStdlib().Seconds() != 45 ||
		config.Gateway.Sessions.IdleTimeout.ToStdlib().Minutes() != 2 ||
		config.Gateway.Sessions.TTL.ToStdlib().Hours() != 4 ||
		config.Gateway.Sessions.MaxPerCodespace != 9 ||
		config.Gateway.Sessions.MaxPerUser != 99 {
		t.Fatalf("gateway session config = %#v", config.Gateway)
	}
	if config.Gateway.SSH.HandshakeTimeout.ToStdlib() != 20*time.Second ||
		config.Gateway.SSH.MaxChannelsPerConnection != 7 ||
		config.Gateway.SSH.Auth.MaxAttemptsPerIP != 11 ||
		config.Gateway.SSH.Auth.MaxAttemptsPerCodespace != 12 ||
		config.Gateway.SSH.Auth.MaxAttemptsPerIPCodespace != 13 ||
		config.Gateway.SSH.Auth.MaxAttemptsPerPublicKey != 14 ||
		config.Gateway.SSH.Auth.FailureWindow.ToStdlib().Minutes() != 5 {
		t.Fatalf("gateway ssh config = %#v", config.Gateway)
	}
	if config.Runtime.Incus.Project.Name != "gitea-codespace" || !config.Runtime.Incus.Project.Manage ||
		config.Runtime.Incus.Storage.Pool != "default" || config.Runtime.Incus.Network.Name != "csnet" || !config.Runtime.Incus.Network.Manage {
		t.Fatalf("incus config = %#v", config.Runtime.Incus)
	}
	if config.Runtime.WebIDE.CodeServerVersion != "4.121.0" {
		t.Fatalf("code-server version = %q", config.Runtime.WebIDE.CodeServerVersion)
	}
	environment := config.Runtime.Environments[0]
	if environment.Tag != "default" || environment.Description != "General development" || normalizeEnvironmentType(environment.Type) != "virtual-machine" || environment.Source.Image != "images:debian/12" || environment.Resources.CPU != 2 {
		t.Fatalf("environment = %#v", environment)
	}
}

func TestConfigRejectsMovingCodeServerVersion(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.Runtime.WebIDE.CodeServerVersion = "latest"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "explicit semantic version") {
		t.Fatalf("code-server version error = %v", err)
	}
}

func TestConfigValidatesRuntimeCache(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.Runtime.Cache = RuntimeCacheConfig{
		Registry: RuntimeCacheRegistryConfig{
			Enabled:     true,
			Listen:      "127.0.0.1:15000",
			PublicURL:   "http://cache.example.com",
			StoragePath: "cache-registry",
			MaxSize:     "10GiB",
			Upstreams: map[string]RuntimeCacheUpstreamConfig{
				"ghcr.io": {
					Allow: []string{"devcontainers/*"},
				},
			},
		},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("valid runtime cache: %v", err)
	}
	config.Runtime.Cache.Registry.PublicURL = "https://registry.example.com?bad=1"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("registry public_url error = %v", err)
	}
	config.Runtime.Cache.Registry.PublicURL = "http://cache.example.com"
	config.Runtime.Cache.Registry.Upstreams["GHCR.IO"] = RuntimeCacheUpstreamConfig{}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "lowercase registry host") {
		t.Fatalf("registry upstream host error = %v", err)
	}
}

func TestLoadCheckedInYAMLConfigs(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		filepath.Join("..", "..", "codespace.yaml"),
		filepath.Join("..", "..", "examples", "config.example.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			config, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("load checked-in config %s: %v", path, err)
			}
			if config.provisionerKind != "incus" {
				t.Fatalf("provisioner kind = %q", config.provisionerKind)
			}
			if len(config.Runtime.Environments) == 0 {
				t.Fatalf("config %s has no runtime environments", path)
			}
		})
	}
}

func TestLoadConfigRejectsJSON(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "codespace.json")
	if err := os.WriteFile(configPath, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatalf("write json config: %v", err)
	}
	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatalf("expected json config error")
	}
	if !strings.Contains(err.Error(), "must be a yaml file") {
		t.Fatalf("json config error = %v", err)
	}
}

func TestLoadConfigRejectsDuplicateEnvironmentTags(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "codespace.yaml")
	content := `
runtime:
  environments:
    - tag: "default"
    - tag: "default"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml config: %v", err)
	}
	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatalf("expected duplicate tag error")
	}
	if !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate tag error = %v", err)
	}
}

func TestLoadConfigInstanceSource(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "codespace.yaml")
	content := `
runtime:
  environments:
    - tag: "environment"
      type: "vm"
      source:
        instance:
          project: "environments"
          name: "dev-environment"
      resources:
        cpu: 2
        memory: "1GiB"
        root_disk: "10GiB"
      profiles: ["default"]
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml config: %v", err)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}
	environment := config.Runtime.Environments[0]
	if environment.Source.Instance == nil ||
		environment.Source.Instance.Project != "environments" ||
		environment.Source.Instance.Name != "dev-environment" {
		t.Fatalf("environment source = %#v", environment)
	}
}

func TestGatewayConfigValidation(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.Gateway.Limits.PublicMaxConnectionsPerEndpoint = 4
	config.Gateway.Limits.PublicMaxConnectionsPerIP = 5
	if err := config.Validate(); err == nil {
		t.Fatalf("expected per-ip gateway limit validation error")
	}

	config = DefaultConfig()
	config.Gateway.Limits.MaxInflightTotal = 4
	config.Gateway.Limits.MaxInflightPerSession = 5
	if err := config.Validate(); err == nil {
		t.Fatalf("expected per-session gateway limit validation error")
	}

	config = DefaultConfig()
	config.Gateway.SSH.MaxChannelsPerConnection = 1025
	if err := config.Validate(); err == nil {
		t.Fatalf("expected ssh max channels validation error")
	}

	config = DefaultConfig()
	config.Gateway.SSH.HandshakeTimeout = Duration(time.Millisecond)
	if err := config.Validate(); err == nil {
		t.Fatalf("expected ssh handshake timeout validation error")
	}

	config = DefaultConfig()
	config.Gateway.Sessions.IdleTimeout = Duration(time.Millisecond)
	if err := config.Validate(); err == nil {
		t.Fatalf("expected session idle timeout validation error")
	}

	config = DefaultConfig()
	config.Node.CapacityTotal = 10001
	if err := config.Validate(); err == nil {
		t.Fatalf("expected capacity total validation error")
	}

	config = DefaultConfig()
	config.Gateway.HTTP.PublicURL = "http://127.0.0.1:18081"
	if err := config.Validate(); err == nil {
		t.Fatalf("expected gateway public url validation error")
	} else if !strings.Contains(err.Error(), "gateway.http.public_url") {
		t.Fatalf("gateway public url validation error = %v", err)
	}

	config = DefaultConfig()
	config.Gateway.SSH.PublicAddr = "127.0.0.1:2222"
	if err := config.Validate(); err == nil {
		t.Fatalf("expected gateway ssh public addr validation error")
	} else if !strings.Contains(err.Error(), "gateway.ssh.public_addr") {
		t.Fatalf("gateway ssh public addr validation error = %v", err)
	}
}
