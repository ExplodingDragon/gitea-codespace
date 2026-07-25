// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	Server      ServerConfig              `json:"server" yaml:"server"`
	Gateway     GatewayConfig             `json:"gateway" yaml:"gateway"`
	Manager     ManagerConfig             `json:"manager" yaml:"manager"`
	Scripts     ScriptsConfig             `json:"scripts" yaml:"scripts"`
	Incus       IncusConfig               `json:"incus" yaml:"incus"`
	Templates   map[string]TemplateConfig `json:"templates" yaml:"templates"`
	Provisioner ProvisionerConfig         `json:"provisioner" yaml:"provisioner"`
}

// ServerConfig stores listener and public URL settings.
type ServerConfig struct {
	ListenAddr           string   `json:"listen_addr" yaml:"listen_addr"`
	GatewayListenAddr    string   `json:"gateway_listen" yaml:"gateway_listen"`
	GatewaySSHListenAddr string   `json:"gateway_ssh_listen" yaml:"gateway_ssh_listen"`
	PublicBaseURL        string   `json:"public_base_url" yaml:"public_base_url"`
	ShutdownTimeout      Duration `json:"shutdown_timeout" yaml:"shutdown_timeout"`
}

// GatewayConfig stores user-facing gateway settings.
type GatewayConfig struct {
	SSHHost                          string   `json:"ssh_host" yaml:"ssh_host"`
	SSHPort                          int      `json:"ssh_port" yaml:"ssh_port"`
	SessionTTL                       Duration `json:"gateway_session_ttl" yaml:"gateway_session_ttl"`
	SessionIdleTimeout               Duration `json:"gateway_session_idle_timeout" yaml:"gateway_session_idle_timeout"`
	SessionRevalidateInterval        Duration `json:"gateway_session_revalidate_interval" yaml:"gateway_session_revalidate_interval"`
	MaxSessionsPerCodespace          int      `json:"gateway_max_sessions_per_codespace" yaml:"gateway_max_sessions_per_codespace"`
	MaxSessionsPerUser               int      `json:"gateway_max_sessions_per_user" yaml:"gateway_max_sessions_per_user"`
	MaxInflightTotal                 int      `json:"gateway_max_inflight_total" yaml:"gateway_max_inflight_total"`
	MaxInflightPerSession            int      `json:"gateway_max_inflight_per_session" yaml:"gateway_max_inflight_per_session"`
	SSHMaxChannelsPerConnection      int      `json:"gateway_ssh_max_channels_per_connection" yaml:"gateway_ssh_max_channels_per_connection"`
	SSHAuthMaxAttemptsPerIP          int      `json:"ssh_auth_max_attempts_per_ip_per_minute" yaml:"ssh_auth_max_attempts_per_ip_per_minute"`
	SSHAuthMaxAttemptsPerCodespace   int      `json:"ssh_auth_max_attempts_per_codespace_per_minute" yaml:"ssh_auth_max_attempts_per_codespace_per_minute"`
	SSHAuthMaxAttemptsPerIPCodespace int      `json:"ssh_auth_max_attempts_per_ip_codespace_per_minute" yaml:"ssh_auth_max_attempts_per_ip_codespace_per_minute"`
	SSHAuthMaxAttemptsPerPublicKey   int      `json:"ssh_auth_max_attempts_per_public_key_per_minute" yaml:"ssh_auth_max_attempts_per_public_key_per_minute"`
	SSHAuthBackoffBase               Duration `json:"ssh_auth_backoff_base" yaml:"ssh_auth_backoff_base"`
	SSHAuthBackoffMax                Duration `json:"ssh_auth_backoff_max" yaml:"ssh_auth_backoff_max"`
	SSHAuthFailureWindow             Duration `json:"ssh_auth_failure_window" yaml:"ssh_auth_failure_window"`
	PublicMaxConnectionsPerEndpoint  int      `json:"gateway_public_max_connections_per_endpoint" yaml:"gateway_public_max_connections_per_endpoint"`
	PublicMaxConnectionsPerIP        int      `json:"gateway_public_max_connections_per_ip" yaml:"gateway_public_max_connections_per_ip"`
	ValidationMaxInflight            int      `json:"gateway_validation_max_inflight" yaml:"gateway_validation_max_inflight"`
}

// ManagerConfig stores embedded manager behavior and capabilities.
type ManagerConfig struct {
	StateDir        string   `json:"state_dir" yaml:"state_dir"`
	Name            string   `json:"name" yaml:"name"`
	GatewayURL      string   `json:"gateway_url" yaml:"gateway_url"`
	GatewaySSHAddr  string   `json:"gateway_ssh_addr" yaml:"gateway_ssh_addr"`
	Version         string   `json:"version" yaml:"version"`
	PollInterval    Duration `json:"poll_interval" yaml:"poll_interval"`
	DeclareInterval Duration `json:"declare_interval" yaml:"declare_interval"`
	CapacityTotal   int32    `json:"capacity_total" yaml:"capacity_total"`
	StartupWorkers  int32    `json:"startup_workers" yaml:"startup_workers"`
	CleanupWorkers  int32    `json:"cleanup_workers" yaml:"cleanup_workers"`
	HTTPTimeout     Duration `json:"http_timeout" yaml:"http_timeout"`
}

// ScriptsConfig stores the create/resume script entry points.
type ScriptsConfig struct {
	Init   string `json:"init" yaml:"init"`
	Start  string `json:"start" yaml:"start"`
	Resume string `json:"resume" yaml:"resume"`
}

// IncusConfig stores Incus connection settings.
type IncusConfig struct {
	Endpoint   string `json:"endpoint" yaml:"endpoint"`
	UnixSocket string `json:"unix_socket" yaml:"unix_socket"`
	Project    string `json:"project" yaml:"project"`
}

// TemplateConfig stores one repository tag to Incus template mapping.
type TemplateConfig struct {
	Image                  string   `json:"image" yaml:"image"`
	InstanceType           string   `json:"instance_type" yaml:"instance_type"`
	CommunicationInterface string   `json:"communication_nic" yaml:"communication_nic"`
	CPU                    int32    `json:"cpu" yaml:"cpu"`
	MemoryLimit            string   `json:"memory" yaml:"memory"`
	RootDiskSize           string   `json:"root_disk_size" yaml:"root_disk_size"`
	Profiles               []string `json:"profiles" yaml:"profiles"`
}

// ProvisionerConfig stores provisioner selection and runtime options.
type ProvisionerConfig struct {
	Kind          string          `json:"kind" yaml:"kind"`
	CodespaceRoot string          `json:"codespace_root" yaml:"codespace_root"`
	Bootstrap     BootstrapConfig `json:"bootstrap" yaml:"bootstrap"`
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
			GatewayListenAddr:    ":18081",
			GatewaySSHListenAddr: ":2222",
			PublicBaseURL:        "http://127.0.0.1:18081",
			ShutdownTimeout:      Duration(10 * time.Second),
		},
		Gateway: GatewayConfig{
			SSHHost:                          "gateway.example.com",
			SSHPort:                          22,
			SessionTTL:                       Duration(8 * time.Hour),
			SessionIdleTimeout:               Duration(30 * time.Minute),
			SessionRevalidateInterval:        Duration(5 * time.Minute),
			MaxSessionsPerCodespace:          32,
			MaxSessionsPerUser:               128,
			MaxInflightTotal:                 4096,
			MaxInflightPerSession:            32,
			SSHMaxChannelsPerConnection:      32,
			SSHAuthMaxAttemptsPerIP:          30,
			SSHAuthMaxAttemptsPerCodespace:   20,
			SSHAuthMaxAttemptsPerIPCodespace: 10,
			SSHAuthMaxAttemptsPerPublicKey:   30,
			SSHAuthBackoffBase:               Duration(time.Second),
			SSHAuthBackoffMax:                Duration(30 * time.Second),
			SSHAuthFailureWindow:             Duration(10 * time.Minute),
			PublicMaxConnectionsPerEndpoint:  64,
			PublicMaxConnectionsPerIP:        16,
			ValidationMaxInflight:            128,
		},
		Manager: ManagerConfig{
			StateDir:        "codespace-state",
			Name:            "codespace-manager",
			Version:         "0.1.0",
			PollInterval:    Duration(750 * time.Millisecond),
			DeclareInterval: Duration(5 * time.Second),
			CapacityTotal:   4,
			StartupWorkers:  4,
			CleanupWorkers:  4,
			HTTPTimeout:     Duration(15 * time.Second),
		},
		Scripts: ScriptsConfig{
			Init:   "builtin",
			Start:  "builtin",
			Resume: "builtin",
		},
		Templates: map[string]TemplateConfig{
			"default": {
				Image:                  "images:debian/12",
				InstanceType:           "container",
				CommunicationInterface: "eth0",
				CPU:                    1,
				MemoryLimit:            "1GiB",
				RootDiskSize:           "10GiB",
				Profiles:               []string{"default"},
			},
		},
		Provisioner: ProvisionerConfig{
			Kind:          "dummy",
			CodespaceRoot: "/codespace",
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
	for _, validate := range []func() error{
		c.Server.Validate,
		c.Manager.Validate,
		c.Provisioner.Validate,
		c.validateTemplates,
		c.Scripts.Validate,
		c.Gateway.Validate,
	} {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) applyDefaults() {
	defaults := DefaultConfig()

	c.Server.applyDefaults(defaults.Server)
	c.Gateway.applyDefaults(defaults.Gateway)
	c.Manager.applyDefaults(defaults.Manager, c.Server, c.Gateway)
	c.Scripts.applyDefaults(defaults.Scripts)
	c.Provisioner.applyDefaults(defaults.Provisioner)
	if len(c.Templates) == 0 {
		c.Templates = cloneTemplates(defaults.Templates)
	}
}

// Validate checks whether the server listeners and public URL are usable.
func (c ServerConfig) Validate() error {
	if strings.TrimSpace(c.GatewayListenAddr) == "" {
		return fmt.Errorf("server.gateway_listen is required")
	}
	if strings.TrimSpace(c.GatewaySSHListenAddr) == "" {
		return fmt.Errorf("server.gateway_ssh_listen is required")
	}
	if strings.TrimSpace(c.PublicBaseURL) == "" {
		return fmt.Errorf("server.public_base_url is required")
	}
	return nil
}

func (c *ServerConfig) applyDefaults(defaults ServerConfig) {
	if strings.TrimSpace(c.ListenAddr) == "" {
		c.ListenAddr = defaults.ListenAddr
	}
	if strings.TrimSpace(c.GatewayListenAddr) == "" {
		c.GatewayListenAddr = defaults.GatewayListenAddr
	}
	if strings.TrimSpace(c.GatewaySSHListenAddr) == "" {
		c.GatewaySSHListenAddr = defaults.GatewaySSHListenAddr
	}
	if strings.TrimSpace(c.PublicBaseURL) == "" {
		c.PublicBaseURL = defaults.PublicBaseURL
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = defaults.ShutdownTimeout
	}
}

// Validate checks whether manager settings are usable.
func (c ManagerConfig) Validate() error {
	if strings.TrimSpace(c.StateDir) == "" {
		return fmt.Errorf("manager.state_dir is required")
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("manager.name is required")
	}
	if strings.TrimSpace(c.GatewayURL) == "" {
		return fmt.Errorf("manager.gateway_url is required")
	}
	if strings.TrimSpace(c.GatewaySSHAddr) == "" {
		return fmt.Errorf("manager.gateway_ssh_addr is required")
	}
	if c.CapacityTotal < 1 || c.CapacityTotal > 10000 {
		return fmt.Errorf("manager.capacity_total must be between 1 and 10000")
	}
	if c.StartupWorkers < 1 || c.StartupWorkers > 256 {
		return fmt.Errorf("manager.startup_workers must be between 1 and 256")
	}
	if c.CleanupWorkers < 1 || c.CleanupWorkers > 256 {
		return fmt.Errorf("manager.cleanup_workers must be between 1 and 256")
	}
	return nil
}

func (c *ManagerConfig) applyDefaults(defaults ManagerConfig, server ServerConfig, gateway GatewayConfig) {
	if strings.TrimSpace(c.StateDir) == "" {
		c.StateDir = defaults.StateDir
	}
	if strings.TrimSpace(c.Name) == "" {
		c.Name = defaults.Name
	}
	if strings.TrimSpace(c.GatewayURL) == "" {
		c.GatewayURL = server.PublicBaseURL
	}
	if strings.TrimSpace(c.Version) == "" {
		c.Version = defaults.Version
	}
	if c.PollInterval == 0 {
		c.PollInterval = defaults.PollInterval
	}
	if strings.TrimSpace(c.GatewaySSHAddr) == "" {
		c.GatewaySSHAddr = gateway.SSHHost
		if gateway.SSHPort > 0 {
			c.GatewaySSHAddr = fmt.Sprintf("%s:%d", c.GatewaySSHAddr, gateway.SSHPort)
		}
	}
	if c.DeclareInterval == 0 {
		c.DeclareInterval = defaults.DeclareInterval
	}
	if c.CapacityTotal == 0 {
		c.CapacityTotal = defaults.CapacityTotal
	}
	if c.StartupWorkers == 0 {
		c.StartupWorkers = minInt32(c.CapacityTotal, defaults.StartupWorkers)
	}
	if c.CleanupWorkers == 0 {
		c.CleanupWorkers = defaults.CleanupWorkers
	}
	if c.HTTPTimeout == 0 {
		c.HTTPTimeout = defaults.HTTPTimeout
	}
}

// Validate checks whether provisioner settings are usable.
func (c ProvisionerConfig) Validate() error {
	if strings.TrimSpace(c.Kind) == "" {
		return fmt.Errorf("provisioner.kind is required")
	}
	return nil
}

func (c *ProvisionerConfig) applyDefaults(defaults ProvisionerConfig) {
	if strings.TrimSpace(c.Kind) == "" {
		c.Kind = defaults.Kind
	}
	if strings.TrimSpace(c.CodespaceRoot) == "" {
		c.CodespaceRoot = defaults.CodespaceRoot
	}
	if strings.TrimSpace(c.Bootstrap.Shell) == "" {
		c.Bootstrap.Shell = defaults.Bootstrap.Shell
	}
	if strings.TrimSpace(c.Bootstrap.HomeDir) == "" {
		c.Bootstrap.HomeDir = defaults.Bootstrap.HomeDir
	}
}

func (c Config) validateTemplates() error {
	if len(c.Templates) == 0 {
		return fmt.Errorf("templates must define at least one tag")
	}
	for tag, template := range c.Templates {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return fmt.Errorf("templates tag must not be empty")
		}
		if err := template.validate(tag); err != nil {
			return err
		}
	}
	return nil
}

func (c TemplateConfig) validate(tag string) error {
	if strings.TrimSpace(c.Image) == "" {
		return fmt.Errorf("templates.%s.image is required", tag)
	}
	switch strings.TrimSpace(c.InstanceType) {
	case "container", "virtual-machine":
	default:
		return fmt.Errorf("templates.%s.instance_type must be container or virtual-machine", tag)
	}
	if strings.TrimSpace(c.CommunicationInterface) == "" {
		return fmt.Errorf("templates.%s.communication_nic is required", tag)
	}
	if c.CPU < 1 {
		return fmt.Errorf("templates.%s.cpu must be positive", tag)
	}
	if strings.TrimSpace(c.MemoryLimit) == "" {
		return fmt.Errorf("templates.%s.memory is required", tag)
	}
	if strings.TrimSpace(c.RootDiskSize) == "" {
		return fmt.Errorf("templates.%s.root_disk_size is required", tag)
	}
	if len(c.Profiles) == 0 {
		return fmt.Errorf("templates.%s.profiles is required", tag)
	}
	return nil
}

// Validate checks whether gateway settings are usable.
func (c GatewayConfig) Validate() error {
	if c.MaxInflightTotal < 1 || c.MaxInflightTotal > 1000000 {
		return fmt.Errorf("gateway.gateway_max_inflight_total must be between 1 and 1000000")
	}
	if c.MaxInflightPerSession < 1 || c.MaxInflightPerSession > 1024 {
		return fmt.Errorf("gateway.gateway_max_inflight_per_session must be between 1 and 1024")
	}
	if c.MaxInflightPerSession > c.MaxInflightTotal {
		return fmt.Errorf("gateway.gateway_max_inflight_per_session must not exceed gateway.gateway_max_inflight_total")
	}
	if c.SSHMaxChannelsPerConnection < 1 || c.SSHMaxChannelsPerConnection > 1024 {
		return fmt.Errorf("gateway.gateway_ssh_max_channels_per_connection must be between 1 and 1024")
	}
	if c.SSHAuthMaxAttemptsPerIP < 1 {
		return fmt.Errorf("gateway.ssh_auth_max_attempts_per_ip_per_minute must be at least 1")
	}
	if c.SSHAuthMaxAttemptsPerCodespace < 1 {
		return fmt.Errorf("gateway.ssh_auth_max_attempts_per_codespace_per_minute must be at least 1")
	}
	if c.SSHAuthMaxAttemptsPerIPCodespace < 1 {
		return fmt.Errorf("gateway.ssh_auth_max_attempts_per_ip_codespace_per_minute must be at least 1")
	}
	if c.SSHAuthMaxAttemptsPerPublicKey < 1 {
		return fmt.Errorf("gateway.ssh_auth_max_attempts_per_public_key_per_minute must be at least 1")
	}
	if base := c.SSHAuthBackoffBase.ToStdlib(); base < time.Second {
		return fmt.Errorf("gateway.ssh_auth_backoff_base must be at least 1s")
	}
	if max := c.SSHAuthBackoffMax.ToStdlib(); max < c.SSHAuthBackoffBase.ToStdlib() {
		return fmt.Errorf("gateway.ssh_auth_backoff_max must not be less than gateway.ssh_auth_backoff_base")
	}
	if window := c.SSHAuthFailureWindow.ToStdlib(); window < time.Minute {
		return fmt.Errorf("gateway.ssh_auth_failure_window must be at least 1m")
	}
	if timeout := c.SessionIdleTimeout.ToStdlib(); timeout < time.Second {
		return fmt.Errorf("gateway.gateway_session_idle_timeout must be at least 1s")
	}
	if ttl := c.SessionTTL.ToStdlib(); ttl < time.Minute {
		return fmt.Errorf("gateway.gateway_session_ttl must be at least 1m")
	}
	if interval := c.SessionRevalidateInterval.ToStdlib(); interval < time.Second || interval > time.Hour {
		return fmt.Errorf("gateway.gateway_session_revalidate_interval must be between 1s and 1h")
	}
	if c.MaxSessionsPerCodespace < 1 || c.MaxSessionsPerCodespace > 10000 {
		return fmt.Errorf("gateway.gateway_max_sessions_per_codespace must be between 1 and 10000")
	}
	if c.MaxSessionsPerUser < 1 || c.MaxSessionsPerUser > 10000 {
		return fmt.Errorf("gateway.gateway_max_sessions_per_user must be between 1 and 10000")
	}
	if c.PublicMaxConnectionsPerEndpoint < 1 || c.PublicMaxConnectionsPerEndpoint > 10000 {
		return fmt.Errorf("gateway.gateway_public_max_connections_per_endpoint must be between 1 and 10000")
	}
	if c.PublicMaxConnectionsPerIP < 1 || c.PublicMaxConnectionsPerIP > 10000 {
		return fmt.Errorf("gateway.gateway_public_max_connections_per_ip must be between 1 and 10000")
	}
	if c.PublicMaxConnectionsPerIP > c.PublicMaxConnectionsPerEndpoint {
		return fmt.Errorf("gateway.gateway_public_max_connections_per_ip must not exceed gateway.gateway_public_max_connections_per_endpoint")
	}
	if c.ValidationMaxInflight < 1 || c.ValidationMaxInflight > 4096 {
		return fmt.Errorf("gateway.gateway_validation_max_inflight must be between 1 and 4096")
	}
	return nil
}

func (c *GatewayConfig) applyDefaults(defaults GatewayConfig) {
	if strings.TrimSpace(c.SSHHost) == "" {
		c.SSHHost = defaults.SSHHost
	}
	if c.SSHPort == 0 {
		c.SSHPort = defaults.SSHPort
	}
	if c.MaxInflightTotal == 0 {
		c.MaxInflightTotal = defaults.MaxInflightTotal
	}
	if c.MaxInflightPerSession == 0 {
		c.MaxInflightPerSession = defaults.MaxInflightPerSession
	}
	if c.SSHMaxChannelsPerConnection == 0 {
		c.SSHMaxChannelsPerConnection = defaults.SSHMaxChannelsPerConnection
	}
	if c.SSHAuthMaxAttemptsPerIP == 0 {
		c.SSHAuthMaxAttemptsPerIP = defaults.SSHAuthMaxAttemptsPerIP
	}
	if c.SSHAuthMaxAttemptsPerCodespace == 0 {
		c.SSHAuthMaxAttemptsPerCodespace = defaults.SSHAuthMaxAttemptsPerCodespace
	}
	if c.SSHAuthMaxAttemptsPerIPCodespace == 0 {
		c.SSHAuthMaxAttemptsPerIPCodespace = defaults.SSHAuthMaxAttemptsPerIPCodespace
	}
	if c.SSHAuthMaxAttemptsPerPublicKey == 0 {
		c.SSHAuthMaxAttemptsPerPublicKey = defaults.SSHAuthMaxAttemptsPerPublicKey
	}
	if c.SSHAuthBackoffBase == 0 {
		c.SSHAuthBackoffBase = defaults.SSHAuthBackoffBase
	}
	if c.SSHAuthBackoffMax == 0 {
		c.SSHAuthBackoffMax = defaults.SSHAuthBackoffMax
	}
	if c.SSHAuthFailureWindow == 0 {
		c.SSHAuthFailureWindow = defaults.SSHAuthFailureWindow
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = defaults.SessionTTL
	}
	if c.SessionIdleTimeout == 0 {
		c.SessionIdleTimeout = defaults.SessionIdleTimeout
	}
	if c.SessionRevalidateInterval == 0 {
		c.SessionRevalidateInterval = defaults.SessionRevalidateInterval
	}
	if c.MaxSessionsPerCodespace == 0 {
		c.MaxSessionsPerCodespace = defaults.MaxSessionsPerCodespace
	}
	if c.MaxSessionsPerUser == 0 {
		c.MaxSessionsPerUser = defaults.MaxSessionsPerUser
	}
	if c.PublicMaxConnectionsPerEndpoint == 0 {
		c.PublicMaxConnectionsPerEndpoint = defaults.PublicMaxConnectionsPerEndpoint
	}
	if c.PublicMaxConnectionsPerIP == 0 {
		c.PublicMaxConnectionsPerIP = defaults.PublicMaxConnectionsPerIP
	}
	if c.ValidationMaxInflight == 0 {
		c.ValidationMaxInflight = defaults.ValidationMaxInflight
	}
}

func (c *ScriptsConfig) applyDefaults(defaults ScriptsConfig) {
	if strings.TrimSpace(c.Init) == "" {
		c.Init = defaults.Init
	}
	if strings.TrimSpace(c.Start) == "" {
		c.Start = defaults.Start
	}
	if strings.TrimSpace(c.Resume) == "" {
		c.Resume = defaults.Resume
	}
}

func (c Config) templateTags() []string {
	tags := make([]string, 0, len(c.Templates))
	for tag := range c.Templates {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return tags
}

func cloneTemplates(templates map[string]TemplateConfig) map[string]TemplateConfig {
	cloned := make(map[string]TemplateConfig, len(templates))
	for tag, template := range templates {
		template.Profiles = append([]string(nil), template.Profiles...)
		cloned[tag] = template
	}
	return cloned
}

func minInt32(left, right int32) int32 {
	if left < right {
		return left
	}
	return right
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
