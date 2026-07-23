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
  runtime_api_listen: ":19090"
  gateway_listen: ":19091"
  gateway_ssh_listen: ":19022"
  public_base_url: "https://codespace.example.com"
gitea:
  url: "https://gitea.example.com"
manager:
  state_dir: "state"
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
	if config.Server.RuntimeAPIListenAddr != ":19090" {
		t.Fatalf("runtime api listen addr = %q", config.Server.RuntimeAPIListenAddr)
	}
	if config.Server.RuntimeAPIURL != "http://127.0.0.1:19090" {
		t.Fatalf("runtime api url = %q", config.Server.RuntimeAPIURL)
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
	if config.Provisioner.Incus.UnixSocket != "/var/lib/incus/unix.socket" {
		t.Fatalf("incus unix socket = %q", config.Provisioner.Incus.UnixSocket)
	}
	if config.Provisioner.Incus.CommunicationInterface != "eth0" {
		t.Fatalf("incus communication interface = %q", config.Provisioner.Incus.CommunicationInterface)
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
    "runtime_api_listen": ":20080",
    "gateway_listen": ":20081",
    "gateway_ssh_listen": ":20022",
    "public_base_url": "http://127.0.0.1:20080"
  },
  "gitea": {
    "url": "http://127.0.0.1:3000"
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
	if config.Server.RuntimeAPIURL != "http://127.0.0.1:20080" {
		t.Fatalf("runtime api url = %q", config.Server.RuntimeAPIURL)
	}
	if config.Provisioner.Bootstrap.Shell != "/bin/sh" {
		t.Fatalf("bootstrap shell = %q", config.Provisioner.Bootstrap.Shell)
	}
	if config.Gateway.MaxInflightTotal != 4096 ||
		config.Gateway.MaxInflightPerSession != 32 ||
		config.Gateway.PublicMaxConnectionsPerEndpoint != 64 ||
		config.Gateway.PublicMaxConnectionsPerIP != 16 ||
		config.Gateway.ValidationMaxInflight != 128 {
		t.Fatalf("gateway defaults = %#v", config.Gateway)
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
	config.Gateway.ValidationMaxInflight = 4097
	if err := config.Validate(); err == nil {
		t.Fatalf("expected validation inflight limit error")
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
