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

	configPath := filepath.Join(t.TempDir(), "codespace.yaml")
	content := `
server:
  listen_addr: ":19090"
  gateway_listen: ":19091"
  gateway_ssh_listen: ":19022"
  public_base_url: "https://codespace.example.com"
gateway:
  gateway_session_ttl: "4h"
  gateway_session_idle_timeout: "2m"
  gateway_session_revalidate_interval: "45s"
  gateway_max_sessions_per_codespace: 9
  gateway_max_sessions_per_user: 99
  gateway_ssh_max_channels_per_connection: 7
  ssh_auth_max_attempts_per_ip_per_minute: 11
  ssh_auth_max_attempts_per_codespace_per_minute: 12
  ssh_auth_max_attempts_per_ip_codespace_per_minute: 13
  ssh_auth_max_attempts_per_public_key_per_minute: 14
  ssh_auth_backoff_base: "2s"
  ssh_auth_backoff_max: "20s"
  ssh_auth_failure_window: "5m"
manager:
  state_dir: "state"
  name: "yaml-manager"
  poll_interval: "1s"
  capacity_total: 3
  startup_workers: 2
  cleanup_workers: 5
provisioner:
  kind: "incus"
incus:
  unix_socket: "/var/lib/incus/unix.socket"
templates:
  default:
    image: "images:debian/12"
    instance_type: "virtual-machine"
    communication_nic: "eth0"
    cpu: 2
    memory: "1GiB"
    root_disk_size: "10GiB"
    profiles:
      - default
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml config: %v", err)
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}

	if config.Server.ListenAddr != ":19090" {
		t.Fatalf("listen addr = %q", config.Server.ListenAddr)
	}
	if config.Server.GatewayListenAddr != ":19091" {
		t.Fatalf("gateway listen addr = %q", config.Server.GatewayListenAddr)
	}
	if config.Server.GatewaySSHListenAddr != ":19022" {
		t.Fatalf("gateway ssh listen addr = %q", config.Server.GatewaySSHListenAddr)
	}
	if config.Manager.GatewayURL != "https://codespace.example.com" {
		t.Fatalf("manager gateway url = %q", config.Manager.GatewayURL)
	}
	if config.Manager.StateDir != filepath.Join(filepath.Dir(configPath), "state") {
		t.Fatalf("manager state dir = %q", config.Manager.StateDir)
	}
	if config.Manager.PollInterval.ToStdlib().Seconds() != 1 {
		t.Fatalf("manager poll interval = %s", config.Manager.PollInterval.ToStdlib())
	}
	if config.Manager.CapacityTotal != 3 || config.Manager.StartupWorkers != 2 || config.Manager.CleanupWorkers != 5 {
		t.Fatalf("manager capacity config = %#v", config.Manager)
	}
	if config.Gateway.SessionRevalidateInterval.ToStdlib().Seconds() != 45 {
		t.Fatalf("gateway session revalidate interval = %s", config.Gateway.SessionRevalidateInterval.ToStdlib())
	}
	if config.Gateway.SessionIdleTimeout.ToStdlib().Minutes() != 2 {
		t.Fatalf("gateway session idle timeout = %s", config.Gateway.SessionIdleTimeout.ToStdlib())
	}
	if config.Gateway.SessionTTL.ToStdlib().Hours() != 4 ||
		config.Gateway.MaxSessionsPerCodespace != 9 ||
		config.Gateway.MaxSessionsPerUser != 99 {
		t.Fatalf("gateway session config = %#v", config.Gateway)
	}
	if config.Gateway.SSHMaxChannelsPerConnection != 7 {
		t.Fatalf("gateway ssh max channels = %d", config.Gateway.SSHMaxChannelsPerConnection)
	}
	if config.Gateway.SSHAuthMaxAttemptsPerIP != 11 ||
		config.Gateway.SSHAuthMaxAttemptsPerCodespace != 12 ||
		config.Gateway.SSHAuthMaxAttemptsPerIPCodespace != 13 ||
		config.Gateway.SSHAuthMaxAttemptsPerPublicKey != 14 ||
		config.Gateway.SSHAuthBackoffBase.ToStdlib().Seconds() != 2 ||
		config.Gateway.SSHAuthBackoffMax.ToStdlib().Seconds() != 20 ||
		config.Gateway.SSHAuthFailureWindow.ToStdlib().Minutes() != 5 {
		t.Fatalf("gateway ssh auth limit config = %#v", config.Gateway)
	}
	if config.Incus.UnixSocket != "/var/lib/incus/unix.socket" {
		t.Fatalf("incus unix socket = %q", config.Incus.UnixSocket)
	}
	template := config.Templates["default"]
	if template.CommunicationInterface != "eth0" {
		t.Fatalf("template communication interface = %q", template.CommunicationInterface)
	}
	if template.Image != "images:debian/12" {
		t.Fatalf("template image = %q", template.Image)
	}
	if template.InstanceType != "virtual-machine" {
		t.Fatalf("template instance type = %q", template.InstanceType)
	}
	if template.CPU != 2 {
		t.Fatalf("template cpu = %d", template.CPU)
	}
	if template.MemoryLimit != "1GiB" {
		t.Fatalf("template memory limit = %q", template.MemoryLimit)
	}
	if template.RootDiskSize != "10GiB" {
		t.Fatalf("template root disk size = %q", template.RootDiskSize)
	}
	if len(template.Profiles) != 1 || template.Profiles[0] != "default" {
		t.Fatalf("template profiles = %#v", template.Profiles)
	}
	if config.Scripts.Init != "builtin" || config.Scripts.Start != "builtin" || config.Scripts.Resume != "builtin" {
		t.Fatalf("scripts defaults = %#v", config.Scripts)
	}
}

func TestLoadConfigJSON(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "codespace.json")
	content := `{
	  "server": {
	    "listen_addr": ":20080",
	    "gateway_listen": ":20081",
    "gateway_ssh_listen": ":20022",
    "public_base_url": "http://127.0.0.1:20080"
  },
  "manager": {
    "state_dir": "state",
    "name": "json-manager",
    "gateway_url": "http://127.0.0.1:20080"
  },
  "provisioner": {
    "kind": "dummy"
  }
}`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write json config: %v", err)
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load json config: %v", err)
	}

	if config.Manager.StateDir != filepath.Join(filepath.Dir(configPath), "state") {
		t.Fatalf("manager state dir = %q", config.Manager.StateDir)
	}
	if config.Server.ShutdownTimeout.ToStdlib().Seconds() != 10 {
		t.Fatalf("shutdown timeout = %s", config.Server.ShutdownTimeout.ToStdlib())
	}
	if config.Provisioner.Bootstrap.Shell != "/bin/sh" {
		t.Fatalf("bootstrap shell = %q", config.Provisioner.Bootstrap.Shell)
	}
	template := config.Templates["default"]
	if template.Image != "images:debian/12" {
		t.Fatalf("default template image = %q", template.Image)
	}
	if template.InstanceType != "container" {
		t.Fatalf("default template instance type = %q", template.InstanceType)
	}
	if config.Manager.CapacityTotal != 4 ||
		config.Manager.StartupWorkers != 4 ||
		config.Manager.CleanupWorkers != 4 {
		t.Fatalf("manager capacity defaults = %#v", config.Manager)
	}
	if config.Gateway.MaxInflightTotal != 4096 ||
		config.Gateway.SessionTTL.ToStdlib().Hours() != 8 ||
		config.Gateway.SessionIdleTimeout.ToStdlib().Minutes() != 30 ||
		config.Gateway.SessionRevalidateInterval.ToStdlib().Minutes() != 5 ||
		config.Gateway.MaxSessionsPerCodespace != 32 ||
		config.Gateway.MaxSessionsPerUser != 128 ||
		config.Gateway.MaxInflightPerSession != 32 ||
		config.Gateway.SSHMaxChannelsPerConnection != 32 ||
		config.Gateway.SSHAuthMaxAttemptsPerIP != 30 ||
		config.Gateway.SSHAuthMaxAttemptsPerCodespace != 20 ||
		config.Gateway.SSHAuthMaxAttemptsPerIPCodespace != 10 ||
		config.Gateway.SSHAuthMaxAttemptsPerPublicKey != 30 ||
		config.Gateway.SSHAuthBackoffBase.ToStdlib().Seconds() != 1 ||
		config.Gateway.SSHAuthBackoffMax.ToStdlib().Seconds() != 30 ||
		config.Gateway.SSHAuthFailureWindow.ToStdlib().Minutes() != 10 ||
		config.Gateway.PublicMaxConnectionsPerEndpoint != 64 ||
		config.Gateway.PublicMaxConnectionsPerIP != 16 ||
		config.Gateway.ValidationMaxInflight != 128 {
		t.Fatalf("gateway defaults = %#v", config.Gateway)
	}
}

func TestLoadConfigRejectsGatewaySSHHostKeyStateFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		content string
	}{
		{
			name: "yaml",
			file: "codespace.yaml",
			content: `
manager:
  gateway_ssh_host_key_algorithm: "ssh-ed25519"
`,
		},
		{
			name: "json",
			file: "codespace.json",
			content: `{
  "manager": {
    "gateway_ssh_host_key_fingerprint_sha256": "SHA256:test"
  }
}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), test.file)
			if err := os.WriteFile(configPath, []byte(test.content), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := LoadConfig(configPath)
			if err == nil {
				t.Fatalf("expected config decode error")
			}
			if !strings.Contains(err.Error(), "gateway_ssh_host_key_") {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
}

func TestLoadConfigRejectsDynamicCapacityStateFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		content string
	}{
		{
			name: "yaml capacity available",
			file: "codespace.yaml",
			content: `
manager:
  capacity_available: 1
`,
		},
		{
			name: "json cleanup capacity available",
			file: "codespace.json",
			content: `{
  "manager": {
    "cleanup_capacity_available": 1
  }
}`,
		},
		{
			name: "yaml max operations",
			file: "codespace.yaml",
			content: `
manager:
  max_operations: 1
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), test.file)
			if err := os.WriteFile(configPath, []byte(test.content), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := LoadConfig(configPath)
			if err == nil {
				t.Fatalf("expected config decode error")
			}
			if !strings.Contains(err.Error(), "capacity_available") &&
				!strings.Contains(err.Error(), "max_operations") {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
}

func TestLoadConfigRejectsRemovedTemplateCompatibilityFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		content string
		want    string
	}{
		{
			name: "manager tags",
			file: "codespace.yaml",
			content: `
manager:
  tags:
    - default
`,
			want: "tags",
		},
		{
			name: "provisioner incus",
			file: "codespace.json",
			content: `{
  "provisioner": {
    "incus": {
      "image": "images:debian/12"
    }
  }
}`,
			want: "incus",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), test.file)
			if err := os.WriteFile(configPath, []byte(test.content), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := LoadConfig(configPath)
			if err == nil {
				t.Fatalf("expected config decode error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decode error = %v", err)
			}
		})
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
	config.Gateway.SessionTTL = Duration(time.Second)
	if err := config.Validate(); err == nil {
		t.Fatalf("expected session ttl validation error")
	}

	config = DefaultConfig()
	config.Gateway.MaxSessionsPerCodespace = 0
	config.applyDefaults()
	config.Gateway.MaxSessionsPerCodespace = -1
	if err := config.Validate(); err == nil {
		t.Fatalf("expected per-codespace session limit validation error")
	}

	config = DefaultConfig()
	config.Gateway.MaxSessionsPerUser = 0
	config.applyDefaults()
	config.Gateway.MaxSessionsPerUser = -1
	if err := config.Validate(); err == nil {
		t.Fatalf("expected per-user session limit validation error")
	}

	config = DefaultConfig()
	config.Gateway.SSHAuthMaxAttemptsPerIP = 0
	config.applyDefaults()
	config.Gateway.SSHAuthMaxAttemptsPerIP = -1
	if err := config.Validate(); err == nil {
		t.Fatalf("expected ssh auth per-ip validation error")
	}

	config = DefaultConfig()
	config.Gateway.SSHAuthBackoffMax = Duration(time.Millisecond)
	if err := config.Validate(); err == nil {
		t.Fatalf("expected ssh auth backoff validation error")
	}

	config = DefaultConfig()
	config.Gateway.SSHAuthFailureWindow = Duration(time.Second)
	if err := config.Validate(); err == nil {
		t.Fatalf("expected ssh auth failure window validation error")
	}

	config = DefaultConfig()
	config.Gateway.ValidationMaxInflight = 4097
	if err := config.Validate(); err == nil {
		t.Fatalf("expected validation inflight limit error")
	}

	config = DefaultConfig()
	config.Manager.CapacityTotal = 10001
	if err := config.Validate(); err == nil {
		t.Fatalf("expected capacity total validation error")
	}

	config = DefaultConfig()
	config.Manager.StartupWorkers = 257
	if err := config.Validate(); err == nil {
		t.Fatalf("expected startup workers validation error")
	}

	config = DefaultConfig()
	config.Manager.CleanupWorkers = 257
	if err := config.Validate(); err == nil {
		t.Fatalf("expected cleanup workers validation error")
	}

	config = DefaultConfig()
	config.Templates["default"] = TemplateConfig{
		Image:                  "images:debian/12",
		InstanceType:           "serverless",
		CommunicationInterface: "eth0",
		CPU:                    1,
		MemoryLimit:            "1GiB",
		RootDiskSize:           "10GiB",
		Profiles:               []string{"default"},
	}
	if err := config.Validate(); err == nil {
		t.Fatalf("expected template instance type validation error")
	}

	config = DefaultConfig()
	config.Templates["default"] = TemplateConfig{
		Image:                  "images:debian/12",
		InstanceType:           "container",
		CommunicationInterface: "eth0",
		MemoryLimit:            "1GiB",
		RootDiskSize:           "10GiB",
		Profiles:               []string{"default"},
	}
	if err := config.Validate(); err == nil {
		t.Fatalf("expected template cpu validation error")
	}

	config = DefaultConfig()
	config.Templates["default"] = TemplateConfig{
		Image:                  "images:debian/12",
		InstanceType:           "container",
		CommunicationInterface: "eth0",
		CPU:                    1,
		MemoryLimit:            "1GiB",
		Profiles:               []string{"default"},
	}
	if err := config.Validate(); err == nil {
		t.Fatalf("expected template root disk validation error")
	}
}

func TestScriptsConfigValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	initPath := writeScriptForTest(t, dir, "init.sh")
	startPath := writeScriptForTest(t, dir, "start.sh")
	resumePath := writeScriptForTest(t, dir, "resume.sh")

	config := DefaultConfig()
	config.applyDefaults()
	config.Scripts = ScriptsConfig{
		Init:   initPath,
		Start:  startPath,
		Resume: resumePath,
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("custom scripts validation: %v", err)
	}

	config = DefaultConfig()
	config.applyDefaults()
	config.Scripts = ScriptsConfig{
		Init:   "builtin",
		Start:  startPath,
		Resume: resumePath,
	}
	if err := config.Validate(); err == nil {
		t.Fatalf("expected mixed scripts validation error")
	}

	config = DefaultConfig()
	config.applyDefaults()
	config.Scripts = ScriptsConfig{
		Init:   "init.sh",
		Start:  startPath,
		Resume: resumePath,
	}
	if err := config.Validate(); err == nil {
		t.Fatalf("expected relative script path validation error")
	}
}

func writeScriptForTest(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write script %s: %v", name, err)
	}
	return path
}
