// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var defaultConfigNames = []string{
	"codespace.yaml",
	"codespace.yml",
	"codespace.json",
}

const defaultRegisterConfigPath = "codespace.yaml"

// Duration stores one configuration duration value.
type Duration time.Duration

// UnmarshalJSON decodes one duration from string or integer nanoseconds.
func (d *Duration) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		parsed, parseErr := time.ParseDuration(text)
		if parseErr != nil {
			return fmt.Errorf("parse duration %q: %w", text, parseErr)
		}
		*d = Duration(parsed)
		return nil
	}

	var raw int64
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode duration: %w", err)
	}
	*d = Duration(time.Duration(raw))
	return nil
}

// UnmarshalYAML decodes one duration from string or integer nanoseconds.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var text string
	if err := node.Decode(&text); err == nil {
		parsed, parseErr := time.ParseDuration(text)
		if parseErr != nil {
			return fmt.Errorf("parse duration %q: %w", text, parseErr)
		}
		*d = Duration(parsed)
		return nil
	}

	var raw int64
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("decode duration: %w", err)
	}
	*d = Duration(time.Duration(raw))
	return nil
}

// MarshalJSON encodes one duration as string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// MarshalYAML encodes one duration as string.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// ToStdlib returns the stdlib duration.
func (d Duration) ToStdlib() time.Duration {
	return time.Duration(d)
}

// Config stores runtime configuration.
type Config struct {
	Server      ServerConfig      `json:"server" yaml:"server"`
	Gitea       GiteaConfig       `json:"gitea" yaml:"gitea"`
	Gateway     GatewayConfig     `json:"gateway" yaml:"gateway"`
	Manager     ManagerConfig     `json:"manager" yaml:"manager"`
	Scripts     ScriptsConfig     `json:"scripts" yaml:"scripts"`
	Provisioner ProvisionerConfig `json:"provisioner" yaml:"provisioner"`
}

// ServerConfig stores listener and public URL settings.
type ServerConfig struct {
	ListenAddr           string   `json:"listen_addr" yaml:"listen_addr"`
	RuntimeAPIListenAddr string   `json:"runtime_api_listen" yaml:"runtime_api_listen"`
	RuntimeAPIURL        string   `json:"runtime_api_url" yaml:"runtime_api_url"`
	GatewayListenAddr    string   `json:"gateway_listen" yaml:"gateway_listen"`
	GatewaySSHListenAddr string   `json:"gateway_ssh_listen" yaml:"gateway_ssh_listen"`
	PublicBaseURL        string   `json:"public_base_url" yaml:"public_base_url"`
	ShutdownTimeout      Duration `json:"shutdown_timeout" yaml:"shutdown_timeout"`
}

// GiteaConfig stores the remote Gitea control-plane endpoint.
type GiteaConfig struct {
	URL string `json:"url" yaml:"url"`
}

// GatewayConfig stores user-facing gateway settings.
type GatewayConfig struct {
	SSHHost                         string `json:"ssh_host" yaml:"ssh_host"`
	SSHPort                         int    `json:"ssh_port" yaml:"ssh_port"`
	MaxInflightTotal                int    `json:"gateway_max_inflight_total" yaml:"gateway_max_inflight_total"`
	MaxInflightPerSession           int    `json:"gateway_max_inflight_per_session" yaml:"gateway_max_inflight_per_session"`
	PublicMaxConnectionsPerEndpoint int    `json:"gateway_public_max_connections_per_endpoint" yaml:"gateway_public_max_connections_per_endpoint"`
	PublicMaxConnectionsPerIP       int    `json:"gateway_public_max_connections_per_ip" yaml:"gateway_public_max_connections_per_ip"`
	ValidationMaxInflight           int    `json:"gateway_validation_max_inflight" yaml:"gateway_validation_max_inflight"`
}

// ManagerConfig stores embedded manager behavior and capabilities.
type ManagerConfig struct {
	StateDir                           string   `json:"state_dir" yaml:"state_dir"`
	Name                               string   `json:"name" yaml:"name"`
	GatewayURL                         string   `json:"gateway_url" yaml:"gateway_url"`
	GatewaySSHAddr                     string   `json:"gateway_ssh_addr" yaml:"gateway_ssh_addr"`
	GatewaySSHHostKeyAlgorithm         string   `json:"gateway_ssh_host_key_algorithm" yaml:"gateway_ssh_host_key_algorithm"`
	GatewaySSHHostKeyFingerprintSHA256 string   `json:"gateway_ssh_host_key_fingerprint_sha256" yaml:"gateway_ssh_host_key_fingerprint_sha256"`
	GatewaySSHHostKeyUpdatedUnix       int64    `json:"gateway_ssh_host_key_updated_unix" yaml:"gateway_ssh_host_key_updated_unix"`
	Version                            string   `json:"version" yaml:"version"`
	PollInterval                       Duration `json:"poll_interval" yaml:"poll_interval"`
	DeclareInterval                    Duration `json:"declare_interval" yaml:"declare_interval"`
	CapacityTotal                      int32    `json:"capacity_total" yaml:"capacity_total"`
	CapacityAvailable                  int32    `json:"capacity_available" yaml:"capacity_available"`
	CleanupCapacityAvailable           int32    `json:"cleanup_capacity_available" yaml:"cleanup_capacity_available"`
	MaxOperations                      int32    `json:"max_operations" yaml:"max_operations"`
	HTTPTimeout                        Duration `json:"http_timeout" yaml:"http_timeout"`
	Tags                               []string `json:"tags" yaml:"tags"`
}

// ScriptsConfig stores the create/resume script entry points.
type ScriptsConfig struct {
	Init   string `json:"init" yaml:"init"`
	Start  string `json:"start" yaml:"start"`
	Resume string `json:"resume" yaml:"resume"`
}

// ProvisionerConfig stores provisioner selection and runtime options.
type ProvisionerConfig struct {
	Kind          string          `json:"kind" yaml:"kind"`
	CodespaceRoot string          `json:"codespace_root" yaml:"codespace_root"`
	Incus         IncusAPIConfig  `json:"incus" yaml:"incus"`
	Bootstrap     BootstrapConfig `json:"bootstrap" yaml:"bootstrap"`
}

// IncusAPIConfig stores Incus connection settings.
type IncusAPIConfig struct {
	Remote                 string `json:"remote" yaml:"remote"`
	UnixSocket             string `json:"unix_socket" yaml:"unix_socket"`
	Project                string `json:"project" yaml:"project"`
	CommunicationInterface string `json:"communication_interface" yaml:"communication_interface"`
}

// BootstrapConfig stores codespace bootstrap execution settings.
type BootstrapConfig struct {
	Shell   string `json:"shell" yaml:"shell"`
	HomeDir string `json:"home_dir" yaml:"home_dir"`
	User    uint32 `json:"user" yaml:"user"`
	Group   uint32 `json:"group" yaml:"group"`
}

// DefaultConfig returns one runnable reference configuration.
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			ListenAddr:           ":18080",
			RuntimeAPIListenAddr: ":18080",
			RuntimeAPIURL:        "http://127.0.0.1:18080",
			GatewayListenAddr:    ":18081",
			GatewaySSHListenAddr: ":2222",
			PublicBaseURL:        "http://127.0.0.1:18081",
			ShutdownTimeout:      Duration(10 * time.Second),
		},
		Gitea: GiteaConfig{
			URL: "http://127.0.0.1:3000",
		},
		Gateway: GatewayConfig{
			SSHHost:                         "gateway.example.com",
			SSHPort:                         22,
			MaxInflightTotal:                4096,
			MaxInflightPerSession:           32,
			PublicMaxConnectionsPerEndpoint: 64,
			PublicMaxConnectionsPerIP:       16,
			ValidationMaxInflight:           128,
		},
		Manager: ManagerConfig{
			StateDir:                 "codespace-state",
			Name:                     "codespace-manager",
			Version:                  "0.1.0",
			PollInterval:             Duration(750 * time.Millisecond),
			DeclareInterval:          Duration(5 * time.Second),
			CapacityTotal:            4,
			CapacityAvailable:        4,
			CleanupCapacityAvailable: 4,
			MaxOperations:            4,
			HTTPTimeout:              Duration(15 * time.Second),
			Tags:                     []string{"default"},
		},
		Scripts: ScriptsConfig{
			Init:   "builtin",
			Start:  "builtin",
			Resume: "builtin",
		},
		Provisioner: ProvisionerConfig{
			Kind:          "dummy",
			CodespaceRoot: "/codespace",
			Incus: IncusAPIConfig{
				CommunicationInterface: "eth0",
			},
			Bootstrap: BootstrapConfig{
				Shell:   "/bin/sh",
				HomeDir: "/root",
			},
		},
	}
}

// DiscoverConfigPath returns one existing config path.
func DiscoverConfigPath(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return path, nil
	}

	for _, candidate := range defaultConfigNames {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("config file not found, tried %s", strings.Join(defaultConfigNames, ", "))
}

// LoadConfig loads one JSON or YAML config file.
func LoadConfig(path string) (Config, error) {
	configPath, err := DiscoverConfigPath(path)
	if err != nil {
		return Config{}, err
	}

	config, err := decodeConfigFile(configPath)
	if err != nil {
		return Config{}, err
	}
	config.applyDefaults()
	config.resolveRelativePaths(configPath)
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", configPath, err)
	}
	return config, nil
}

// LoadConfigForRegister loads an existing config without requiring manager credentials.
func LoadConfigForRegister(path string) (Config, error) {
	configPath, err := DiscoverConfigPath(path)
	if err != nil {
		config := DefaultConfig()
		config.applyDefaults()
		config.resolveRelativePaths(path)
		return config, nil
	}
	config, err := decodeConfigFile(configPath)
	if err != nil {
		return Config{}, err
	}
	config.applyDefaults()
	config.resolveRelativePaths(configPath)
	return config, nil
}

// SaveRegisterConfig writes the registered manager configuration as YAML.
func SaveRegisterConfig(path string, config Config) error {
	if strings.TrimSpace(path) == "" {
		path = defaultRegisterConfigPath
	}
	content, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

func decodeConfigFile(configPath string) (Config, error) {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", configPath, err)
	}

	config := DefaultConfig()
	switch strings.ToLower(filepath.Ext(configPath)) {
	case ".json":
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config); err != nil {
			return Config{}, fmt.Errorf("decode json config %s: %w", configPath, err)
		}
	default:
		decoder := yaml.NewDecoder(bytes.NewReader(content))
		decoder.KnownFields(true)
		if err := decoder.Decode(&config); err != nil {
			return Config{}, fmt.Errorf("decode yaml config %s: %w", configPath, err)
		}
	}
	return config, nil
}

// Validate checks whether the config is usable.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.RuntimeAPIListenAddr) == "" {
		return fmt.Errorf("server.runtime_api_listen is required")
	}
	if strings.TrimSpace(c.Server.RuntimeAPIURL) == "" {
		return fmt.Errorf("server.runtime_api_url is required")
	}
	if strings.TrimSpace(c.Server.GatewayListenAddr) == "" {
		return fmt.Errorf("server.gateway_listen is required")
	}
	if strings.TrimSpace(c.Server.GatewaySSHListenAddr) == "" {
		return fmt.Errorf("server.gateway_ssh_listen is required")
	}
	if strings.TrimSpace(c.Server.PublicBaseURL) == "" {
		return fmt.Errorf("server.public_base_url is required")
	}
	if strings.TrimSpace(c.Gitea.URL) == "" {
		return fmt.Errorf("gitea.url is required")
	}
	if strings.TrimSpace(c.Manager.StateDir) == "" {
		return fmt.Errorf("manager.state_dir is required")
	}
	if strings.TrimSpace(c.Manager.Name) == "" {
		return fmt.Errorf("manager.name is required")
	}
	if strings.TrimSpace(c.Manager.GatewayURL) == "" {
		return fmt.Errorf("manager.gateway_url is required")
	}
	if strings.TrimSpace(c.Manager.GatewaySSHAddr) == "" {
		return fmt.Errorf("manager.gateway_ssh_addr is required")
	}
	if strings.TrimSpace(c.Provisioner.Kind) == "" {
		return fmt.Errorf("provisioner.kind is required")
	}
	if err := c.Scripts.Validate(); err != nil {
		return err
	}
	if c.Gateway.MaxInflightTotal < 1 || c.Gateway.MaxInflightTotal > 1000000 {
		return fmt.Errorf("gateway.gateway_max_inflight_total must be between 1 and 1000000")
	}
	if c.Gateway.MaxInflightPerSession < 1 || c.Gateway.MaxInflightPerSession > 1024 {
		return fmt.Errorf("gateway.gateway_max_inflight_per_session must be between 1 and 1024")
	}
	if c.Gateway.MaxInflightPerSession > c.Gateway.MaxInflightTotal {
		return fmt.Errorf("gateway.gateway_max_inflight_per_session must not exceed gateway.gateway_max_inflight_total")
	}
	if c.Gateway.PublicMaxConnectionsPerEndpoint < 1 || c.Gateway.PublicMaxConnectionsPerEndpoint > 10000 {
		return fmt.Errorf("gateway.gateway_public_max_connections_per_endpoint must be between 1 and 10000")
	}
	if c.Gateway.PublicMaxConnectionsPerIP < 1 || c.Gateway.PublicMaxConnectionsPerIP > 10000 {
		return fmt.Errorf("gateway.gateway_public_max_connections_per_ip must be between 1 and 10000")
	}
	if c.Gateway.PublicMaxConnectionsPerIP > c.Gateway.PublicMaxConnectionsPerEndpoint {
		return fmt.Errorf("gateway.gateway_public_max_connections_per_ip must not exceed gateway.gateway_public_max_connections_per_endpoint")
	}
	if c.Gateway.ValidationMaxInflight < 1 || c.Gateway.ValidationMaxInflight > 4096 {
		return fmt.Errorf("gateway.gateway_validation_max_inflight must be between 1 and 4096")
	}
	return nil
}

func (c *Config) applyDefaults() {
	defaults := DefaultConfig()

	if strings.TrimSpace(c.Server.ListenAddr) == "" {
		c.Server.ListenAddr = defaults.Server.ListenAddr
	}
	if strings.TrimSpace(c.Server.RuntimeAPIListenAddr) == "" {
		if strings.TrimSpace(c.Server.ListenAddr) != "" {
			c.Server.RuntimeAPIListenAddr = c.Server.ListenAddr
		} else {
			c.Server.RuntimeAPIListenAddr = defaults.Server.RuntimeAPIListenAddr
		}
	}
	if strings.TrimSpace(c.Server.RuntimeAPIURL) == "" ||
		(c.Server.RuntimeAPIURL == defaults.Server.RuntimeAPIURL && c.Server.RuntimeAPIListenAddr != defaults.Server.RuntimeAPIListenAddr) {
		c.Server.RuntimeAPIURL = runtimeAPIURLFromListen(c.Server.RuntimeAPIListenAddr)
	}
	if strings.TrimSpace(c.Server.GatewayListenAddr) == "" {
		c.Server.GatewayListenAddr = defaults.Server.GatewayListenAddr
	}
	if strings.TrimSpace(c.Server.GatewaySSHListenAddr) == "" {
		c.Server.GatewaySSHListenAddr = defaults.Server.GatewaySSHListenAddr
	}
	if strings.TrimSpace(c.Server.PublicBaseURL) == "" {
		c.Server.PublicBaseURL = defaults.Server.PublicBaseURL
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = defaults.Server.ShutdownTimeout
	}
	if strings.TrimSpace(c.Gitea.URL) == "" {
		c.Gitea.URL = defaults.Gitea.URL
	}
	if strings.TrimSpace(c.Gateway.SSHHost) == "" {
		c.Gateway.SSHHost = defaults.Gateway.SSHHost
	}
	if c.Gateway.SSHPort == 0 {
		c.Gateway.SSHPort = defaults.Gateway.SSHPort
	}
	if c.Gateway.MaxInflightTotal == 0 {
		c.Gateway.MaxInflightTotal = defaults.Gateway.MaxInflightTotal
	}
	if c.Gateway.MaxInflightPerSession == 0 {
		c.Gateway.MaxInflightPerSession = defaults.Gateway.MaxInflightPerSession
	}
	if c.Gateway.PublicMaxConnectionsPerEndpoint == 0 {
		c.Gateway.PublicMaxConnectionsPerEndpoint = defaults.Gateway.PublicMaxConnectionsPerEndpoint
	}
	if c.Gateway.PublicMaxConnectionsPerIP == 0 {
		c.Gateway.PublicMaxConnectionsPerIP = defaults.Gateway.PublicMaxConnectionsPerIP
	}
	if c.Gateway.ValidationMaxInflight == 0 {
		c.Gateway.ValidationMaxInflight = defaults.Gateway.ValidationMaxInflight
	}
	if strings.TrimSpace(c.Manager.StateDir) == "" {
		c.Manager.StateDir = defaults.Manager.StateDir
	}
	if strings.TrimSpace(c.Manager.Name) == "" {
		c.Manager.Name = defaults.Manager.Name
	}
	if strings.TrimSpace(c.Manager.GatewayURL) == "" {
		c.Manager.GatewayURL = c.Server.PublicBaseURL
	}
	if strings.TrimSpace(c.Manager.Version) == "" {
		c.Manager.Version = defaults.Manager.Version
	}
	if c.Manager.PollInterval == 0 {
		c.Manager.PollInterval = defaults.Manager.PollInterval
	}
	if strings.TrimSpace(c.Manager.GatewaySSHAddr) == "" {
		c.Manager.GatewaySSHAddr = c.Gateway.SSHHost
		if c.Gateway.SSHPort > 0 {
			c.Manager.GatewaySSHAddr = fmt.Sprintf("%s:%d", c.Manager.GatewaySSHAddr, c.Gateway.SSHPort)
		}
	}
	if c.Manager.DeclareInterval == 0 {
		c.Manager.DeclareInterval = defaults.Manager.DeclareInterval
	}
	if c.Manager.CapacityTotal == 0 {
		c.Manager.CapacityTotal = defaults.Manager.CapacityTotal
	}
	if c.Manager.CapacityAvailable == 0 {
		c.Manager.CapacityAvailable = defaults.Manager.CapacityAvailable
	}
	if c.Manager.CleanupCapacityAvailable == 0 {
		c.Manager.CleanupCapacityAvailable = defaults.Manager.CleanupCapacityAvailable
	}
	if c.Manager.MaxOperations == 0 {
		c.Manager.MaxOperations = defaults.Manager.MaxOperations
	}
	if c.Manager.HTTPTimeout == 0 {
		c.Manager.HTTPTimeout = defaults.Manager.HTTPTimeout
	}
	if len(c.Manager.Tags) == 0 {
		c.Manager.Tags = append([]string(nil), defaults.Manager.Tags...)
	}
	if strings.TrimSpace(c.Scripts.Init) == "" {
		c.Scripts.Init = defaults.Scripts.Init
	}
	if strings.TrimSpace(c.Scripts.Start) == "" {
		c.Scripts.Start = defaults.Scripts.Start
	}
	if strings.TrimSpace(c.Scripts.Resume) == "" {
		c.Scripts.Resume = defaults.Scripts.Resume
	}
	if strings.TrimSpace(c.Provisioner.Kind) == "" {
		c.Provisioner.Kind = defaults.Provisioner.Kind
	}
	if strings.TrimSpace(c.Provisioner.CodespaceRoot) == "" {
		c.Provisioner.CodespaceRoot = defaults.Provisioner.CodespaceRoot
	}
	if strings.TrimSpace(c.Provisioner.Incus.CommunicationInterface) == "" {
		c.Provisioner.Incus.CommunicationInterface = defaults.Provisioner.Incus.CommunicationInterface
	}
	if strings.TrimSpace(c.Provisioner.Bootstrap.Shell) == "" {
		c.Provisioner.Bootstrap.Shell = defaults.Provisioner.Bootstrap.Shell
	}
	if strings.TrimSpace(c.Provisioner.Bootstrap.HomeDir) == "" {
		c.Provisioner.Bootstrap.HomeDir = defaults.Provisioner.Bootstrap.HomeDir
	}
}

func runtimeAPIURLFromListen(listenAddr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil || port == "" {
		return DefaultConfig().Server.RuntimeAPIURL
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (c *Config) resolveRelativePaths(configPath string) {
	if strings.TrimSpace(configPath) == "" || filepath.IsAbs(c.Manager.StateDir) {
		return
	}
	configDir := filepath.Dir(configPath)
	if configDir == "." || configDir == "" {
		return
	}
	c.Manager.StateDir = filepath.Clean(filepath.Join(configDir, c.Manager.StateDir))
}

// Validate checks whether the script entry points are usable.
func (c ScriptsConfig) Validate() error {
	entries := []struct {
		name  string
		value string
	}{
		{name: "scripts.init", value: c.Init},
		{name: "scripts.start", value: c.Start},
		{name: "scripts.resume", value: c.Resume},
	}
	builtinCount := 0
	customCount := 0
	for _, entry := range entries {
		value := strings.TrimSpace(entry.value)
		if value == "builtin" {
			builtinCount++
			continue
		}
		customCount++
		if !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be builtin or an absolute local file path", entry.name)
		}
		info, err := os.Stat(value)
		if err != nil {
			return fmt.Errorf("%s file %s is not accessible: %w", entry.name, value, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s file %s must be a regular file", entry.name, value)
		}
	}
	if builtinCount != 0 && customCount != 0 {
		return fmt.Errorf("scripts.init, scripts.start, and scripts.resume must all be builtin or all be absolute local file paths")
	}
	return nil
}
