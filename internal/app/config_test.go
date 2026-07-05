// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigYAML(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "codespace.yaml")
	content := `
server:
  listen_addr: ":19090"
  public_base_url: "https://codespace.example.com"
gitea:
  url: "https://gitea.example.com"
manager:
  uuid: "mgr-yaml"
  token: "manager-token"
  name: "yaml-manager"
  poll_interval: "1s"
provisioner:
  kind: "incus"
  incus:
    unix_socket: "/var/lib/incus/unix.socket"
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
	if config.Manager.GatewayURL != "https://codespace.example.com" {
		t.Fatalf("manager gateway url = %q", config.Manager.GatewayURL)
	}
	if config.Manager.PollInterval.ToStdlib().Seconds() != 1 {
		t.Fatalf("manager poll interval = %s", config.Manager.PollInterval.ToStdlib())
	}
	if config.Provisioner.Incus.UnixSocket != "/var/lib/incus/unix.socket" {
		t.Fatalf("incus unix socket = %q", config.Provisioner.Incus.UnixSocket)
	}
}

func TestLoadConfigJSON(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "codespace.json")
	content := `{
  "server": {
    "listen_addr": ":20080",
    "public_base_url": "http://127.0.0.1:20080"
  },
  "gitea": {
    "url": "http://127.0.0.1:3000"
  },
  "manager": {
    "uuid": "mgr-json",
    "token": "manager-token",
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

	if config.Manager.UUID != "mgr-json" {
		t.Fatalf("manager uuid = %q", config.Manager.UUID)
	}
	if config.Server.ShutdownTimeout.ToStdlib().Seconds() != 10 {
		t.Fatalf("shutdown timeout = %s", config.Server.ShutdownTimeout.ToStdlib())
	}
	if config.Provisioner.Bootstrap.Shell != "/bin/sh" {
		t.Fatalf("bootstrap shell = %q", config.Provisioner.Bootstrap.Shell)
	}
}
