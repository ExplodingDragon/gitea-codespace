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
version: 1

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
    max_channels_per_connection: 7
    auth:
      max_attempts_per_ip_per_minute: 11
      max_attempts_per_codespace_per_minute: 12
      max_attempts_per_ip_codespace_per_minute: 13
      max_attempts_per_public_key_per_minute: 14
      backoff_base: "2s"
      backoff_max: "20s"
      failure_window: "5m"
  sessions:
    ttl: "4h"
    idle_timeout: "2m"
    revalidate_interval: "45s"
    max_per_codespace: 9
    max_per_user: 99

runtime:
  driver: "incus"
  incus:
    connect:
      unix_socket: "/var/lib/incus/unix.socket"
    project:
      name: "gitea-codespace"
      manage: true
    storage:
      pool: "default"
    network:
      name: "gitea-codespace-net"
      manage: true
  environments:
    - tag: "default"
      display_name: "Debian VM"
      type: "vm"
      communication_interface: "eth0"
      source:
        type: "image"
        image: "images:debian/12"
      resources:
        cpu: 2
        memory: "1GiB"
        root_disk: "10GiB"
      profiles:
        use:
          - default
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml config: %v", err)
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}

	if config.Version != 1 {
		t.Fatalf("version = %d", config.Version)
	}
	if config.Manager.StateDir != filepath.Join(dir, "state") {
		t.Fatalf("manager state dir = %q", config.Manager.StateDir)
	}
	if config.Manager.Name != "yaml-manager" || config.Manager.GatewayURL != "https://codespace.example.com" {
		t.Fatalf("manager config = %#v", config.Manager)
	}
	if config.Manager.GatewaySSHAddr != "codespace.example.com:2222" {
		t.Fatalf("manager gateway ssh addr = %q", config.Manager.GatewaySSHAddr)
	}
	if config.Server.GatewayListenAddr != ":19091" || config.Server.GatewaySSHListenAddr != ":19022" {
		t.Fatalf("server listeners = %#v", config.Server)
	}
	if config.Gateway.SessionRevalidateInterval.ToStdlib().Seconds() != 45 ||
		config.Gateway.SessionIdleTimeout.ToStdlib().Minutes() != 2 ||
		config.Gateway.SessionTTL.ToStdlib().Hours() != 4 ||
		config.Gateway.MaxSessionsPerCodespace != 9 ||
		config.Gateway.MaxSessionsPerUser != 99 {
		t.Fatalf("gateway session config = %#v", config.Gateway)
	}
	if config.Gateway.SSHMaxChannelsPerConnection != 7 ||
		config.Gateway.SSHAuthMaxAttemptsPerIP != 11 ||
		config.Gateway.SSHAuthMaxAttemptsPerCodespace != 12 ||
		config.Gateway.SSHAuthMaxAttemptsPerIPCodespace != 13 ||
		config.Gateway.SSHAuthMaxAttemptsPerPublicKey != 14 ||
		config.Gateway.SSHAuthBackoffBase.ToStdlib().Seconds() != 2 ||
		config.Gateway.SSHAuthBackoffMax.ToStdlib().Seconds() != 20 ||
		config.Gateway.SSHAuthFailureWindow.ToStdlib().Minutes() != 5 {
		t.Fatalf("gateway ssh config = %#v", config.Gateway)
	}
	if config.Incus.Project != "gitea-codespace" || !config.Incus.ProjectManage ||
		config.Incus.StoragePool != "default" || config.Incus.NetworkName != "gitea-codespace-net" || !config.Incus.NetworkManage {
		t.Fatalf("incus config = %#v", config.Incus)
	}
	environment := config.RuntimeEnvironments["default"]
	if environment.InstanceType != "virtual-machine" || environment.Image != "images:debian/12" || environment.CPU != 2 {
		t.Fatalf("environment = %#v", environment)
	}
}

func TestLoadCheckedInYAMLConfigs(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		filepath.Join("..", "..", "codespace.yaml"),
		filepath.Join("..", "..", "examples", "config.example.yaml"),
	} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			config, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("load checked-in config %s: %v", path, err)
			}
			if config.Provisioner.Kind != "incus" {
				t.Fatalf("provisioner kind = %q", config.Provisioner.Kind)
			}
			if len(config.RuntimeEnvironments) == 0 {
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
  driver: "incus"
  environments:
    - tag: "environment"
      type: "vm"
      communication_interface: "eth0"
      source:
        type: "instance"
        remote: "local"
        project: "environments"
        name: "dev-environment"
      resources:
        cpu: 2
        memory: "1GiB"
        root_disk: "10GiB"
      profiles:
        use:
          - default
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml config: %v", err)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}
	environment := config.RuntimeEnvironments["environment"]
	if environment.SourceType != "instance" ||
		environment.SourceRemote != "local" ||
		environment.SourceProject != "environments" ||
		environment.SourceName != "dev-environment" {
		t.Fatalf("environment source = %#v", environment)
	}
}

func TestGatewayConfigValidation(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.Gateway.PublicMaxConnectionsPerEndpoint = 4
	config.Gateway.PublicMaxConnectionsPerIP = 5
	if err := config.Validate(); err == nil {
		t.Fatalf("expected per-ip gateway limit validation error")
	}

	config = DefaultConfig()
	config.Gateway.MaxInflightTotal = 4
	config.Gateway.MaxInflightPerSession = 5
	if err := config.Validate(); err == nil {
		t.Fatalf("expected per-session gateway limit validation error")
	}

	config = DefaultConfig()
	config.Gateway.SSHMaxChannelsPerConnection = 1025
	if err := config.Validate(); err == nil {
		t.Fatalf("expected ssh max channels validation error")
	}

	config = DefaultConfig()
	config.Gateway.SessionIdleTimeout = Duration(time.Millisecond)
	if err := config.Validate(); err == nil {
		t.Fatalf("expected session idle timeout validation error")
	}

	config = DefaultConfig()
	config.Manager.CapacityTotal = 10001
	if err := config.Validate(); err == nil {
		t.Fatalf("expected capacity total validation error")
	}

	config = DefaultConfig()
	config.Manager.GatewayURL = "http://127.0.0.1:18081"
	if err := config.Validate(); err == nil {
		t.Fatalf("expected gateway public url validation error")
	} else if !strings.Contains(err.Error(), "gateway.http.public_url") {
		t.Fatalf("gateway public url validation error = %v", err)
	}

	config = DefaultConfig()
	config.Manager.GatewaySSHAddr = "127.0.0.1:2222"
	if err := config.Validate(); err == nil {
		t.Fatalf("expected gateway ssh public addr validation error")
	} else if !strings.Contains(err.Error(), "gateway.ssh.public_addr") {
		t.Fatalf("gateway ssh public addr validation error = %v", err)
	}
}

func TestScriptsConfigValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	initPath := writeScriptForTest(t, dir, "init.sh")
	startPath := writeScriptForTest(t, dir, "start.sh")
	stopPath := writeScriptForTest(t, dir, "stop.sh")

	config := DefaultConfig()
	config.applyDefaults()
	config.Scripts = ScriptsConfig{
		Init:  initPath,
		Start: startPath,
		Stop:  stopPath,
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("custom scripts validation: %v", err)
	}

	config = DefaultConfig()
	config.applyDefaults()
	config.Scripts = ScriptsConfig{
		Init:  "builtin",
		Start: startPath,
		Stop:  stopPath,
	}
	if err := config.Validate(); err == nil {
		t.Fatalf("expected mixed scripts validation error")
	}
}

func writeScriptForTest(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write script %s: %v", name, err)
	}
	return path
}
