// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var defaultConfigNames = []string{
	"codespace.yaml",
	"codespace.yml",
}

const defaultRegisterConfigPath = "codespace.yaml"

var gatewayDNSLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Duration stores one configuration duration value.
type Duration time.Duration

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
	Version int           `yaml:"version"`
	Node    NodeConfig    `yaml:"node"`
	Gateway GatewayConfig `yaml:"gateway"`
	Runtime RuntimeConfig `yaml:"runtime"`

	Server              ServerConfig                        `yaml:"-"`
	Manager             ManagerConfig                       `yaml:"-"`
	Scripts             ScriptsConfig                       `yaml:"-"`
	Incus               IncusConfig                         `yaml:"-"`
	RuntimeEnvironments map[string]RuntimeEnvironmentConfig `yaml:"-"`
	Provisioner         ProvisionerConfig                   `yaml:"-"`
}

// NodeConfig stores manager node behavior and local state.
type NodeConfig struct {
	StateDir        string   `yaml:"state_dir"`
	Name            string   `yaml:"name"`
	Version         string   `yaml:"version"`
	PollInterval    Duration `yaml:"poll_interval"`
	DeclareInterval Duration `yaml:"declare_interval"`
	CapacityTotal   int32    `yaml:"capacity_total"`
	StartupWorkers  int32    `yaml:"startup_workers"`
	CleanupWorkers  int32    `yaml:"cleanup_workers"`
	HTTPTimeout     Duration `yaml:"http_timeout"`
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
}

// ServerConfig stores listener and public URL settings.
type ServerConfig struct {
	ListenAddr           string
	GatewayListenAddr    string
	GatewaySSHListenAddr string
	PublicBaseURL        string
	ShutdownTimeout      Duration
}

// GatewayConfig stores user-facing gateway settings.
type GatewayConfig struct {
	HTTP     GatewayHTTPConfig    `yaml:"http"`
	SSH      GatewaySSHConfig     `yaml:"ssh"`
	Sessions GatewaySessionConfig `yaml:"sessions"`
	Limits   GatewayLimitsConfig  `yaml:"limits"`

	SSHHost                          string   `yaml:"-"`
	SSHPort                          int      `yaml:"-"`
	SessionTTL                       Duration `yaml:"-"`
	SessionIdleTimeout               Duration `yaml:"-"`
	SessionRevalidateInterval        Duration `yaml:"-"`
	MaxSessionsPerCodespace          int      `yaml:"-"`
	MaxSessionsPerUser               int      `yaml:"-"`
	MaxInflightTotal                 int      `yaml:"-"`
	MaxInflightPerSession            int      `yaml:"-"`
	SSHMaxChannelsPerConnection      int      `yaml:"-"`
	SSHAuthMaxAttemptsPerIP          int      `yaml:"-"`
	SSHAuthMaxAttemptsPerCodespace   int      `yaml:"-"`
	SSHAuthMaxAttemptsPerIPCodespace int      `yaml:"-"`
	SSHAuthMaxAttemptsPerPublicKey   int      `yaml:"-"`
	SSHAuthBackoffBase               Duration `yaml:"-"`
	SSHAuthBackoffMax                Duration `yaml:"-"`
	SSHAuthFailureWindow             Duration `yaml:"-"`
	PublicMaxConnectionsPerEndpoint  int      `yaml:"-"`
	PublicMaxConnectionsPerIP        int      `yaml:"-"`
	ValidationMaxInflight            int      `yaml:"-"`
}

// GatewayHTTPConfig stores gateway HTTP listener and public URL.
type GatewayHTTPConfig struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url"`
}

// GatewaySSHConfig stores gateway SSH listener, public address and SSH limits.
type GatewaySSHConfig struct {
	Listen                   string               `yaml:"listen"`
	PublicAddr               string               `yaml:"public_addr"`
	MaxChannelsPerConnection int                  `yaml:"max_channels_per_connection"`
	Auth                     GatewaySSHAuthConfig `yaml:"auth"`
}

// GatewaySSHAuthConfig stores SSH authentication rate limits.
type GatewaySSHAuthConfig struct {
	MaxAttemptsPerIP          int      `yaml:"max_attempts_per_ip_per_minute"`
	MaxAttemptsPerCodespace   int      `yaml:"max_attempts_per_codespace_per_minute"`
	MaxAttemptsPerIPCodespace int      `yaml:"max_attempts_per_ip_codespace_per_minute"`
	MaxAttemptsPerPublicKey   int      `yaml:"max_attempts_per_public_key_per_minute"`
	BackoffBase               Duration `yaml:"backoff_base"`
	BackoffMax                Duration `yaml:"backoff_max"`
	FailureWindow             Duration `yaml:"failure_window"`
}

// GatewaySessionConfig stores browser and SSH session limits.
type GatewaySessionConfig struct {
	TTL                Duration `yaml:"ttl"`
	IdleTimeout        Duration `yaml:"idle_timeout"`
	RevalidateInterval Duration `yaml:"revalidate_interval"`
	MaxPerCodespace    int      `yaml:"max_per_codespace"`
	MaxPerUser         int      `yaml:"max_per_user"`
}

// GatewayLimitsConfig stores gateway request and connection limits.
type GatewayLimitsConfig struct {
	MaxInflightTotal                int `yaml:"max_inflight_total"`
	MaxInflightPerSession           int `yaml:"max_inflight_per_session"`
	PublicMaxConnectionsPerEndpoint int `yaml:"public_max_connections_per_endpoint"`
	PublicMaxConnectionsPerIP       int `yaml:"public_max_connections_per_ip"`
	ValidationMaxInflight           int `yaml:"validation_max_inflight"`
}

// ManagerConfig stores embedded manager behavior and capabilities.
type ManagerConfig struct {
	StateDir        string
	Name            string
	GatewayURL      string
	GatewaySSHAddr  string
	Version         string
	PollInterval    Duration
	DeclareInterval Duration
	CapacityTotal   int32
	StartupWorkers  int32
	CleanupWorkers  int32
	HTTPTimeout     Duration
}

// ScriptsConfig stores the init/start/stop script entry points.
type ScriptsConfig struct {
	Init  string `yaml:"init"`
	Start string `yaml:"start"`
	Stop  string `yaml:"stop"`
}

// IncusConfig stores Incus connection settings.
type IncusConfig struct {
	Endpoint      string
	UnixSocket    string
	Project       string
	ProjectManage bool
	StoragePool   string
	NetworkName   string
	NetworkManage bool
}

// RuntimeEnvironmentConfig stores one repository tag to an internal runtime environment mapping.
type RuntimeEnvironmentConfig struct {
	Image         string
	InstanceType  string
	CPU           int32
	MemoryLimit   string
	RootDiskSize  string
	Profiles      []string
	SourceType    string
	SourceRemote  string
	SourceProject string
	SourceName    string
}

// ProvisionerConfig stores provisioner selection and runtime options.
type ProvisionerConfig struct {
	Kind          string
	CodespaceRoot string
	Bootstrap     BootstrapConfig
}

// BootstrapConfig stores codespace bootstrap execution settings.
type BootstrapConfig struct {
	Shell    string `yaml:"shell"`
	HomeDir  string `yaml:"home_dir"`
	UserName string `yaml:"user_name"`
	User     uint32 `yaml:"user"`
	Group    uint32 `yaml:"group"`
}

// RuntimeConfig stores backend driver and runtime environments.
type RuntimeConfig struct {
	Driver        string              `yaml:"driver"`
	CodespaceRoot string              `yaml:"codespace_root"`
	Bootstrap     BootstrapConfig     `yaml:"bootstrap"`
	Git           RuntimeGitConfig    `yaml:"git"`
	Scripts       ScriptsConfig       `yaml:"scripts"`
	Incus         RuntimeIncusConfig  `yaml:"incus"`
	Environments  []EnvironmentConfig `yaml:"environments"`
}

// RuntimeGitConfig stores Manager-local Git credential generation settings.
type RuntimeGitConfig struct {
	SSHKeyType string `yaml:"ssh_key_type"`
}

// RuntimeIncusConfig stores Incus connection and namespace settings.
type RuntimeIncusConfig struct {
	Connect RuntimeIncusConnectConfig `yaml:"connect"`
	Project RuntimeIncusProjectConfig `yaml:"project"`
	Storage RuntimeIncusStorageConfig `yaml:"storage"`
	Network RuntimeIncusNetworkConfig `yaml:"network"`
}

// RuntimeIncusConnectConfig stores Incus client connection settings.
type RuntimeIncusConnectConfig struct {
	UnixSocket string `yaml:"unix_socket"`
	RemoteAddr string `yaml:"remote_addr"`
}

// RuntimeIncusProjectConfig stores the Incus project used as namespace.
type RuntimeIncusProjectConfig struct {
	Name   string `yaml:"name"`
	Manage bool   `yaml:"manage"`
}

// RuntimeIncusStorageConfig stores host storage selection.
type RuntimeIncusStorageConfig struct {
	Pool string `yaml:"pool"`
}

// RuntimeIncusNetworkConfig stores the shared managed bridge network selection.
type RuntimeIncusNetworkConfig struct {
	Name   string `yaml:"name"`
	Manage bool   `yaml:"manage"`
}

// EnvironmentConfig stores one Gitea tag to a backend runtime environment.
type EnvironmentConfig struct {
	Tag         string                     `yaml:"tag"`
	DisplayName string                     `yaml:"display_name"`
	Type        string                     `yaml:"type"`
	Source      EnvironmentSourceConfig    `yaml:"source"`
	Resources   EnvironmentResourcesConfig `yaml:"resources"`
	Profiles    EnvironmentProfilesConfig  `yaml:"profiles"`
}

// EnvironmentSourceConfig stores the base used to create one runtime.
type EnvironmentSourceConfig struct {
	Type    string `yaml:"type"`
	Image   string `yaml:"image"`
	Remote  string `yaml:"remote"`
	Project string `yaml:"project"`
	Name    string `yaml:"name"`
}

// EnvironmentResourcesConfig stores runtime resource limits.
type EnvironmentResourcesConfig struct {
	CPU      int32  `yaml:"cpu"`
	Memory   string `yaml:"memory"`
	RootDisk string `yaml:"root_disk"`
}

// EnvironmentProfilesConfig stores project-local Incus profiles.
type EnvironmentProfilesConfig struct {
	Use []string `yaml:"use"`
}

// DefaultConfig returns one runnable reference configuration.
func DefaultConfig() Config {
	config := Config{
		Version: 1,
		Node: NodeConfig{
			StateDir:        "codespace-state",
			Name:            "codespace-manager",
			Version:         "0.1.0",
			PollInterval:    Duration(750 * time.Millisecond),
			DeclareInterval: Duration(5 * time.Second),
			CapacityTotal:   4,
			StartupWorkers:  4,
			CleanupWorkers:  4,
			HTTPTimeout:     Duration(15 * time.Second),
			ShutdownTimeout: Duration(10 * time.Second),
		},
		Gateway: GatewayConfig{
			HTTP: GatewayHTTPConfig{
				Listen:    ":18081",
				PublicURL: "http://gateway.example.com:18081",
			},
			SSH: GatewaySSHConfig{
				Listen:                   ":2222",
				PublicAddr:               "gateway.example.com:22",
				MaxChannelsPerConnection: 32,
				Auth: GatewaySSHAuthConfig{
					MaxAttemptsPerIP:          30,
					MaxAttemptsPerCodespace:   20,
					MaxAttemptsPerIPCodespace: 10,
					MaxAttemptsPerPublicKey:   30,
					BackoffBase:               Duration(time.Second),
					BackoffMax:                Duration(30 * time.Second),
					FailureWindow:             Duration(10 * time.Minute),
				},
			},
			Sessions: GatewaySessionConfig{
				TTL:                Duration(8 * time.Hour),
				IdleTimeout:        Duration(30 * time.Minute),
				RevalidateInterval: Duration(5 * time.Minute),
				MaxPerCodespace:    32,
				MaxPerUser:         128,
			},
			Limits: GatewayLimitsConfig{
				MaxInflightTotal:                4096,
				MaxInflightPerSession:           32,
				PublicMaxConnectionsPerEndpoint: 64,
				PublicMaxConnectionsPerIP:       16,
				ValidationMaxInflight:           128,
			},
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
		Runtime: RuntimeConfig{
			Driver:        "dummy",
			CodespaceRoot: "/codespace",
			Bootstrap: BootstrapConfig{
				Shell:    "/bin/bash",
				HomeDir:  "/root",
				UserName: "codespace",
			},
			Git: RuntimeGitConfig{
				SSHKeyType: "ed25519",
			},
			Scripts: ScriptsConfig{
				Init:  "builtin",
				Start: "builtin",
				Stop:  "builtin",
			},
			Incus: RuntimeIncusConfig{
				Connect: RuntimeIncusConnectConfig{
					UnixSocket: "/var/lib/incus/unix.socket",
				},
				Project: RuntimeIncusProjectConfig{
					Name:   "gitea-codespace",
					Manage: true,
				},
				Storage: RuntimeIncusStorageConfig{
					Pool: "default",
				},
				Network: RuntimeIncusNetworkConfig{
					Name:   "csnet",
					Manage: true,
				},
			},
			Environments: []EnvironmentConfig{
				{
					Tag:         "default",
					DisplayName: "Default",
					Type:        "container",
					Source: EnvironmentSourceConfig{
						Type:  "image",
						Image: "images:debian/12",
					},
					Resources: EnvironmentResourcesConfig{
						CPU:      1,
						Memory:   "1GiB",
						RootDisk: "10GiB",
					},
					Profiles: EnvironmentProfilesConfig{Use: []string{"default"}},
				},
			},
		},
	}
	config.syncRuntimeFields()
	return config
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

// LoadConfig loads one YAML config file.
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
	case ".yaml", ".yml", "":
	default:
		return Config{}, fmt.Errorf("config %s must be a yaml file", configPath)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode yaml config %s: %w", configPath, err)
	}
	return config, nil
}

// Validate checks whether the config is usable.
func (c Config) Validate() error {
	for _, validate := range []func() error{
		c.validateVersion,
		c.validateEnvironments,
		c.validateRuntimeGit,
		c.validateRuntimeIncus,
		c.Server.Validate,
		c.Manager.Validate,
		c.Provisioner.Validate,
		c.validateRuntimeEnvironments,
		c.Scripts.Validate,
		c.Gateway.Validate,
	} {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) validateRuntimeIncus() error {
	if strings.ToLower(strings.TrimSpace(c.Runtime.Driver)) != "incus" {
		return nil
	}
	if c.Runtime.Incus.Network.Manage && !c.Runtime.Incus.Project.Manage {
		return fmt.Errorf("runtime.incus.network.manage requires runtime.incus.project.manage")
	}
	return nil
}

func (c Config) validateRuntimeGit() error {
	switch strings.ToLower(strings.TrimSpace(c.Runtime.Git.SSHKeyType)) {
	case "ed25519", "rsa-4096":
		return nil
	default:
		return fmt.Errorf("runtime.git.ssh_key_type must be ed25519 or rsa-4096")
	}
}

func (c Config) validateVersion() error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1")
	}
	return nil
}

func (c Config) validateEnvironments() error {
	if len(c.Runtime.Environments) == 0 {
		return fmt.Errorf("runtime.environments must define at least one environment")
	}
	seen := map[string]struct{}{}
	for _, environment := range c.Runtime.Environments {
		tag := strings.TrimSpace(environment.Tag)
		if tag == "" {
			return fmt.Errorf("runtime.environments tag must not be empty")
		}
		if _, ok := seen[tag]; ok {
			return fmt.Errorf("runtime.environments tag %q is duplicated", tag)
		}
		seen[tag] = struct{}{}
	}
	return nil
}

func (c *Config) applyDefaults() {
	defaults := DefaultConfig()

	if c.Version == 0 {
		c.Version = defaults.Version
	}
	c.Node.applyDefaults(defaults.Node)
	c.Gateway.applyDefaults(defaults.Gateway)
	c.Runtime.applyDefaults(defaults.Runtime)
	c.syncRuntimeFields()
}

func (c *Config) syncRuntimeFields() {
	c.Server = ServerConfig{
		ListenAddr:           c.Gateway.HTTP.Listen,
		GatewayListenAddr:    c.Gateway.HTTP.Listen,
		GatewaySSHListenAddr: c.Gateway.SSH.Listen,
		PublicBaseURL:        c.Gateway.HTTP.PublicURL,
		ShutdownTimeout:      c.Node.ShutdownTimeout,
	}
	c.Manager = ManagerConfig{
		StateDir:        c.Node.StateDir,
		Name:            c.Node.Name,
		GatewayURL:      c.Gateway.HTTP.PublicURL,
		GatewaySSHAddr:  c.Gateway.SSH.PublicAddr,
		Version:         c.Node.Version,
		PollInterval:    c.Node.PollInterval,
		DeclareInterval: c.Node.DeclareInterval,
		CapacityTotal:   c.Node.CapacityTotal,
		StartupWorkers:  c.Node.StartupWorkers,
		CleanupWorkers:  c.Node.CleanupWorkers,
		HTTPTimeout:     c.Node.HTTPTimeout,
	}
	c.Scripts = c.Runtime.Scripts
	c.Provisioner = ProvisionerConfig{
		Kind:          c.Runtime.Driver,
		CodespaceRoot: c.Runtime.CodespaceRoot,
		Bootstrap:     c.Runtime.Bootstrap,
	}
	c.Incus = IncusConfig{
		Endpoint:      c.Runtime.Incus.Connect.RemoteAddr,
		UnixSocket:    c.Runtime.Incus.Connect.UnixSocket,
		Project:       c.Runtime.Incus.Project.Name,
		ProjectManage: c.Runtime.Incus.Project.Manage,
		StoragePool:   c.Runtime.Incus.Storage.Pool,
		NetworkName:   c.Runtime.Incus.Network.Name,
		NetworkManage: c.Runtime.Incus.Network.Manage,
	}
	c.RuntimeEnvironments = runtimeEnvironmentsFromConfig(c.Runtime.Environments)
}

func (c Config) runtimeGitSSHKeyType() string {
	switch strings.ToLower(strings.TrimSpace(c.Runtime.Git.SSHKeyType)) {
	case "", "ed25519":
		return "ed25519"
	case "rsa-4096":
		return "rsa-4096"
	default:
		return strings.TrimSpace(c.Runtime.Git.SSHKeyType)
	}
}

func (c *NodeConfig) applyDefaults(defaults NodeConfig) {
	if strings.TrimSpace(c.StateDir) == "" {
		c.StateDir = defaults.StateDir
	}
	if strings.TrimSpace(c.Name) == "" {
		c.Name = defaults.Name
	}
	if strings.TrimSpace(c.Version) == "" {
		c.Version = defaults.Version
	}
	if c.PollInterval == 0 {
		c.PollInterval = defaults.PollInterval
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
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = defaults.ShutdownTimeout
	}
}

// Validate checks whether the server listeners and public URL are usable.
func (c ServerConfig) Validate() error {
	if strings.TrimSpace(c.GatewayListenAddr) == "" {
		return fmt.Errorf("gateway.http.listen is required")
	}
	if strings.TrimSpace(c.GatewaySSHListenAddr) == "" {
		return fmt.Errorf("gateway.ssh.listen is required")
	}
	if strings.TrimSpace(c.PublicBaseURL) == "" {
		return fmt.Errorf("gateway.http.public_url is required")
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
		return fmt.Errorf("node.state_dir is required")
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("node.name is required")
	}
	if strings.TrimSpace(c.GatewayURL) == "" {
		return fmt.Errorf("gateway.http.public_url is required")
	}
	if _, err := normalizeManagerGatewayURL(c.GatewayURL); err != nil {
		return fmt.Errorf("gateway.http.public_url is invalid: %w", err)
	}
	if strings.TrimSpace(c.GatewaySSHAddr) == "" {
		return fmt.Errorf("gateway.ssh.public_addr is required")
	}
	if _, err := normalizeManagerGatewaySSHAddr(c.GatewaySSHAddr); err != nil {
		return fmt.Errorf("gateway.ssh.public_addr is invalid: %w", err)
	}
	if c.CapacityTotal < 1 || c.CapacityTotal > 10000 {
		return fmt.Errorf("node.capacity_total must be between 1 and 10000")
	}
	if c.StartupWorkers < 1 || c.StartupWorkers > 256 {
		return fmt.Errorf("node.startup_workers must be between 1 and 256")
	}
	if c.CleanupWorkers < 1 || c.CleanupWorkers > 256 {
		return fmt.Errorf("node.cleanup_workers must be between 1 and 256")
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
		return fmt.Errorf("runtime.driver is required")
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

func (c Config) validateRuntimeEnvironments() error {
	if len(c.RuntimeEnvironments) == 0 {
		return fmt.Errorf("runtime.environments must define at least one environment")
	}
	for tag, environment := range c.RuntimeEnvironments {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return fmt.Errorf("runtime.environments tag must not be empty")
		}
		if err := environment.validate(tag); err != nil {
			return err
		}
	}
	return nil
}

func (c RuntimeEnvironmentConfig) validate(tag string) error {
	switch strings.TrimSpace(c.InstanceType) {
	case "container", "virtual-machine":
	default:
		return fmt.Errorf("runtime.environments.%s.type must be lxc or vm", tag)
	}
	sourceType := strings.TrimSpace(c.SourceType)
	if sourceType == "" {
		sourceType = "image"
	}
	switch sourceType {
	case "image":
		if strings.TrimSpace(c.Image) == "" {
			return fmt.Errorf("runtime.environments.%s.source.image is required", tag)
		}
	case "instance":
		if strings.TrimSpace(c.SourceName) == "" {
			return fmt.Errorf("runtime.environments.%s.source.name is required", tag)
		}
	default:
		return fmt.Errorf("runtime.environments.%s.source.type must be image or instance", tag)
	}
	if c.CPU < 1 {
		return fmt.Errorf("runtime.environments.%s.resources.cpu must be positive", tag)
	}
	if strings.TrimSpace(c.MemoryLimit) == "" {
		return fmt.Errorf("runtime.environments.%s.resources.memory is required", tag)
	}
	if strings.TrimSpace(c.RootDiskSize) == "" {
		return fmt.Errorf("runtime.environments.%s.resources.root_disk is required", tag)
	}
	if len(c.Profiles) == 0 {
		return fmt.Errorf("runtime.environments.%s.profiles.use is required", tag)
	}
	return nil
}

// Validate checks whether gateway settings are usable.
func (c GatewayConfig) Validate() error {
	if c.MaxInflightTotal < 1 || c.MaxInflightTotal > 1000000 {
		return fmt.Errorf("gateway.limits.max_inflight_total must be between 1 and 1000000")
	}
	if c.MaxInflightPerSession < 1 || c.MaxInflightPerSession > 1024 {
		return fmt.Errorf("gateway.limits.max_inflight_per_session must be between 1 and 1024")
	}
	if c.MaxInflightPerSession > c.MaxInflightTotal {
		return fmt.Errorf("gateway.limits.max_inflight_per_session must not exceed gateway.limits.max_inflight_total")
	}
	if c.SSHMaxChannelsPerConnection < 1 || c.SSHMaxChannelsPerConnection > 1024 {
		return fmt.Errorf("gateway.ssh.max_channels_per_connection must be between 1 and 1024")
	}
	if c.SSHAuthMaxAttemptsPerIP < 1 {
		return fmt.Errorf("gateway.ssh.auth.max_attempts_per_ip_per_minute must be at least 1")
	}
	if c.SSHAuthMaxAttemptsPerCodespace < 1 {
		return fmt.Errorf("gateway.ssh.auth.max_attempts_per_codespace_per_minute must be at least 1")
	}
	if c.SSHAuthMaxAttemptsPerIPCodespace < 1 {
		return fmt.Errorf("gateway.ssh.auth.max_attempts_per_ip_codespace_per_minute must be at least 1")
	}
	if c.SSHAuthMaxAttemptsPerPublicKey < 1 {
		return fmt.Errorf("gateway.ssh.auth.max_attempts_per_public_key_per_minute must be at least 1")
	}
	if base := c.SSHAuthBackoffBase.ToStdlib(); base < time.Second {
		return fmt.Errorf("gateway.ssh.auth.backoff_base must be at least 1s")
	}
	if max := c.SSHAuthBackoffMax.ToStdlib(); max < c.SSHAuthBackoffBase.ToStdlib() {
		return fmt.Errorf("gateway.ssh.auth.backoff_max must not be less than gateway.ssh.auth.backoff_base")
	}
	if window := c.SSHAuthFailureWindow.ToStdlib(); window < time.Minute {
		return fmt.Errorf("gateway.ssh.auth.failure_window must be at least 1m")
	}
	if timeout := c.SessionIdleTimeout.ToStdlib(); timeout < time.Second {
		return fmt.Errorf("gateway.sessions.idle_timeout must be at least 1s")
	}
	if ttl := c.SessionTTL.ToStdlib(); ttl < time.Minute {
		return fmt.Errorf("gateway.sessions.ttl must be at least 1m")
	}
	if interval := c.SessionRevalidateInterval.ToStdlib(); interval < time.Second || interval > time.Hour {
		return fmt.Errorf("gateway.sessions.revalidate_interval must be between 1s and 1h")
	}
	if c.MaxSessionsPerCodespace < 1 || c.MaxSessionsPerCodespace > 10000 {
		return fmt.Errorf("gateway.sessions.max_per_codespace must be between 1 and 10000")
	}
	if c.MaxSessionsPerUser < 1 || c.MaxSessionsPerUser > 10000 {
		return fmt.Errorf("gateway.sessions.max_per_user must be between 1 and 10000")
	}
	if c.PublicMaxConnectionsPerEndpoint < 1 || c.PublicMaxConnectionsPerEndpoint > 10000 {
		return fmt.Errorf("gateway.limits.public_max_connections_per_endpoint must be between 1 and 10000")
	}
	if c.PublicMaxConnectionsPerIP < 1 || c.PublicMaxConnectionsPerIP > 10000 {
		return fmt.Errorf("gateway.limits.public_max_connections_per_ip must be between 1 and 10000")
	}
	if c.PublicMaxConnectionsPerIP > c.PublicMaxConnectionsPerEndpoint {
		return fmt.Errorf("gateway.limits.public_max_connections_per_ip must not exceed gateway.limits.public_max_connections_per_endpoint")
	}
	if c.ValidationMaxInflight < 1 || c.ValidationMaxInflight > 4096 {
		return fmt.Errorf("gateway.limits.validation_max_inflight must be between 1 and 4096")
	}
	return nil
}

func (c *GatewayConfig) applyDefaults(defaults GatewayConfig) {
	if strings.TrimSpace(c.HTTP.Listen) == "" {
		c.HTTP.Listen = defaults.HTTP.Listen
	}
	if strings.TrimSpace(c.HTTP.PublicURL) == "" {
		c.HTTP.PublicURL = defaults.HTTP.PublicURL
	}
	if strings.TrimSpace(c.SSH.Listen) == "" {
		c.SSH.Listen = defaults.SSH.Listen
	}
	if strings.TrimSpace(c.SSH.PublicAddr) == "" {
		c.SSH.PublicAddr = defaults.SSH.PublicAddr
	}
	if c.SSH.MaxChannelsPerConnection == 0 {
		c.SSH.MaxChannelsPerConnection = defaults.SSH.MaxChannelsPerConnection
	}
	if c.SSH.Auth.MaxAttemptsPerIP == 0 {
		c.SSH.Auth.MaxAttemptsPerIP = defaults.SSH.Auth.MaxAttemptsPerIP
	}
	if c.SSH.Auth.MaxAttemptsPerCodespace == 0 {
		c.SSH.Auth.MaxAttemptsPerCodespace = defaults.SSH.Auth.MaxAttemptsPerCodespace
	}
	if c.SSH.Auth.MaxAttemptsPerIPCodespace == 0 {
		c.SSH.Auth.MaxAttemptsPerIPCodespace = defaults.SSH.Auth.MaxAttemptsPerIPCodespace
	}
	if c.SSH.Auth.MaxAttemptsPerPublicKey == 0 {
		c.SSH.Auth.MaxAttemptsPerPublicKey = defaults.SSH.Auth.MaxAttemptsPerPublicKey
	}
	if c.SSH.Auth.BackoffBase == 0 {
		c.SSH.Auth.BackoffBase = defaults.SSH.Auth.BackoffBase
	}
	if c.SSH.Auth.BackoffMax == 0 {
		c.SSH.Auth.BackoffMax = defaults.SSH.Auth.BackoffMax
	}
	if c.SSH.Auth.FailureWindow == 0 {
		c.SSH.Auth.FailureWindow = defaults.SSH.Auth.FailureWindow
	}
	if c.Sessions.TTL == 0 {
		c.Sessions.TTL = defaults.Sessions.TTL
	}
	if c.Sessions.IdleTimeout == 0 {
		c.Sessions.IdleTimeout = defaults.Sessions.IdleTimeout
	}
	if c.Sessions.RevalidateInterval == 0 {
		c.Sessions.RevalidateInterval = defaults.Sessions.RevalidateInterval
	}
	if c.Sessions.MaxPerCodespace == 0 {
		c.Sessions.MaxPerCodespace = defaults.Sessions.MaxPerCodespace
	}
	if c.Sessions.MaxPerUser == 0 {
		c.Sessions.MaxPerUser = defaults.Sessions.MaxPerUser
	}
	if c.Limits.MaxInflightTotal == 0 {
		c.Limits.MaxInflightTotal = defaults.Limits.MaxInflightTotal
	}
	if c.Limits.MaxInflightPerSession == 0 {
		c.Limits.MaxInflightPerSession = defaults.Limits.MaxInflightPerSession
	}
	if c.Limits.PublicMaxConnectionsPerEndpoint == 0 {
		c.Limits.PublicMaxConnectionsPerEndpoint = defaults.Limits.PublicMaxConnectionsPerEndpoint
	}
	if c.Limits.PublicMaxConnectionsPerIP == 0 {
		c.Limits.PublicMaxConnectionsPerIP = defaults.Limits.PublicMaxConnectionsPerIP
	}
	if c.Limits.ValidationMaxInflight == 0 {
		c.Limits.ValidationMaxInflight = defaults.Limits.ValidationMaxInflight
	}
	c.SSHHost, c.SSHPort = splitGatewaySSHPublicAddr(c.SSH.PublicAddr)
	c.SessionTTL = c.Sessions.TTL
	c.SessionIdleTimeout = c.Sessions.IdleTimeout
	c.SessionRevalidateInterval = c.Sessions.RevalidateInterval
	c.MaxSessionsPerCodespace = c.Sessions.MaxPerCodespace
	c.MaxSessionsPerUser = c.Sessions.MaxPerUser
	c.MaxInflightTotal = c.Limits.MaxInflightTotal
	c.MaxInflightPerSession = c.Limits.MaxInflightPerSession
	c.SSHMaxChannelsPerConnection = c.SSH.MaxChannelsPerConnection
	c.SSHAuthMaxAttemptsPerIP = c.SSH.Auth.MaxAttemptsPerIP
	c.SSHAuthMaxAttemptsPerCodespace = c.SSH.Auth.MaxAttemptsPerCodespace
	c.SSHAuthMaxAttemptsPerIPCodespace = c.SSH.Auth.MaxAttemptsPerIPCodespace
	c.SSHAuthMaxAttemptsPerPublicKey = c.SSH.Auth.MaxAttemptsPerPublicKey
	c.SSHAuthBackoffBase = c.SSH.Auth.BackoffBase
	c.SSHAuthBackoffMax = c.SSH.Auth.BackoffMax
	c.SSHAuthFailureWindow = c.SSH.Auth.FailureWindow
	c.PublicMaxConnectionsPerEndpoint = c.Limits.PublicMaxConnectionsPerEndpoint
	c.PublicMaxConnectionsPerIP = c.Limits.PublicMaxConnectionsPerIP
	c.ValidationMaxInflight = c.Limits.ValidationMaxInflight
}

func splitGatewaySSHPublicAddr(value string) (string, int) {
	host, portText, ok := strings.Cut(strings.TrimSpace(value), ":")
	if !ok {
		return strings.TrimSpace(value), 0
	}
	var port int
	_, _ = fmt.Sscanf(portText, "%d", &port)
	return host, port
}

func normalizeManagerGatewayURL(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse gateway url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("gateway url must use http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("gateway url host is required")
	}
	if err := validateManagerGatewayDNSHost(host); err != nil {
		return "", fmt.Errorf("invalid gateway url host: %w", err)
	}
	if len(strings.Repeat("a", 30)+"-"+strings.Repeat("0", 32)+"."+host) > 253 {
		return "", fmt.Errorf("derived gateway endpoint host is too long")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("gateway url must not contain userinfo, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("gateway url must not contain a business path")
	}
	port := parsed.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("invalid gateway url port")
		}
		if (parsed.Scheme == "http" && portNumber == 80) || (parsed.Scheme == "https" && portNumber == 443) {
			port = ""
		} else {
			port = strconv.Itoa(portNumber)
		}
	}
	normalized := parsed.Scheme + "://" + host
	if port != "" {
		normalized += ":" + port
	}
	if len(normalized) > 512 {
		return "", fmt.Errorf("gateway url is too long")
	}
	return normalized, nil
}

func normalizeManagerGatewaySSHAddr(rawAddr string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(rawAddr))
	if err != nil {
		return "", fmt.Errorf("gateway ssh address must use host:port")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if err := validateManagerGatewayDNSHost(host); err != nil {
		return "", fmt.Errorf("invalid gateway ssh host: %w", err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("invalid gateway ssh port")
	}
	normalized := net.JoinHostPort(host, strconv.Itoa(portNumber))
	if len(normalized) > 512 {
		return "", fmt.Errorf("gateway ssh address is too long")
	}
	return normalized, nil
}

func validateManagerGatewayDNSHost(host string) error {
	if host == "" || strings.HasSuffix(host, ".") {
		return fmt.Errorf("host must be a DNS name without trailing dot")
	}
	if net.ParseIP(host) != nil {
		return fmt.Errorf("host must not be an IP address")
	}
	if len(host) > 253 {
		return fmt.Errorf("host is too long")
	}
	for label := range strings.SplitSeq(host, ".") {
		if !gatewayDNSLabelPattern.MatchString(label) {
			return fmt.Errorf("invalid DNS label %q", label)
		}
	}
	return nil
}

func (c *RuntimeConfig) applyDefaults(defaults RuntimeConfig) {
	if strings.TrimSpace(c.Driver) == "" {
		c.Driver = defaults.Driver
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
	if strings.TrimSpace(c.Bootstrap.UserName) == "" {
		c.Bootstrap.UserName = defaults.Bootstrap.UserName
	}
	if strings.TrimSpace(c.Git.SSHKeyType) == "" {
		c.Git.SSHKeyType = defaults.Git.SSHKeyType
	}
	c.Scripts.applyDefaults(defaults.Scripts)
	if strings.TrimSpace(c.Incus.Connect.UnixSocket) == "" && strings.TrimSpace(c.Incus.Connect.RemoteAddr) == "" {
		c.Incus.Connect.UnixSocket = defaults.Incus.Connect.UnixSocket
	}
	if strings.TrimSpace(c.Incus.Project.Name) == "" {
		c.Incus.Project.Name = defaults.Incus.Project.Name
	}
	if strings.TrimSpace(c.Incus.Storage.Pool) == "" {
		c.Incus.Storage.Pool = defaults.Incus.Storage.Pool
	}
	if strings.TrimSpace(c.Incus.Network.Name) == "" {
		c.Incus.Network.Name = defaults.Incus.Network.Name
	}
	if len(c.Environments) == 0 {
		c.Environments = cloneEnvironments(defaults.Environments)
	}
	for i := range c.Environments {
		c.Environments[i].applyDefaults(defaults.Environments[0])
	}
}

func (c *EnvironmentConfig) applyDefaults(defaults EnvironmentConfig) {
	if strings.TrimSpace(c.Tag) == "" {
		c.Tag = defaults.Tag
	}
	if strings.TrimSpace(c.DisplayName) == "" {
		c.DisplayName = c.Tag
	}
	if strings.TrimSpace(c.Type) == "" {
		c.Type = defaults.Type
	}
	if strings.TrimSpace(c.Source.Type) == "" {
		c.Source.Type = defaults.Source.Type
	}
	if strings.TrimSpace(c.Source.Type) == "image" && strings.TrimSpace(c.Source.Image) == "" {
		c.Source.Image = defaults.Source.Image
	}
	if c.Resources.CPU == 0 {
		c.Resources.CPU = defaults.Resources.CPU
	}
	if strings.TrimSpace(c.Resources.Memory) == "" {
		c.Resources.Memory = defaults.Resources.Memory
	}
	if strings.TrimSpace(c.Resources.RootDisk) == "" {
		c.Resources.RootDisk = defaults.Resources.RootDisk
	}
	if len(c.Profiles.Use) == 0 {
		c.Profiles.Use = append([]string(nil), defaults.Profiles.Use...)
	}
}

func (c *ScriptsConfig) applyDefaults(defaults ScriptsConfig) {
	if strings.TrimSpace(c.Init) == "" {
		c.Init = defaults.Init
	}
	if strings.TrimSpace(c.Start) == "" {
		c.Start = defaults.Start
	}
	if strings.TrimSpace(c.Stop) == "" {
		c.Stop = defaults.Stop
	}
}

func (c Config) environmentTags() []string {
	tags := make([]string, 0, len(c.RuntimeEnvironments))
	for tag := range c.RuntimeEnvironments {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return tags
}

func runtimeEnvironmentsFromConfig(environments []EnvironmentConfig) map[string]RuntimeEnvironmentConfig {
	result := make(map[string]RuntimeEnvironmentConfig, len(environments))
	for _, environment := range environments {
		tag := strings.TrimSpace(environment.Tag)
		if tag == "" {
			continue
		}
		result[tag] = RuntimeEnvironmentConfig{
			Image:         strings.TrimSpace(environment.Source.Image),
			InstanceType:  normalizeEnvironmentType(environment.Type),
			CPU:           environment.Resources.CPU,
			MemoryLimit:   strings.TrimSpace(environment.Resources.Memory),
			RootDiskSize:  strings.TrimSpace(environment.Resources.RootDisk),
			Profiles:      normalizedConfigProfiles(environment.Profiles.Use),
			SourceType:    strings.TrimSpace(environment.Source.Type),
			SourceRemote:  strings.TrimSpace(environment.Source.Remote),
			SourceProject: strings.TrimSpace(environment.Source.Project),
			SourceName:    strings.TrimSpace(environment.Source.Name),
		}
	}
	return result
}

func normalizeEnvironmentType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "lxc", "container":
		return "container"
	case "vm", "virtual-machine":
		return "virtual-machine"
	default:
		return ""
	}
}

func normalizedConfigProfiles(profiles []string) []string {
	normalized := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		profile = strings.TrimSpace(profile)
		if profile != "" {
			normalized = append(normalized, profile)
		}
	}
	return normalized
}

func cloneEnvironments(environments []EnvironmentConfig) []EnvironmentConfig {
	cloned := make([]EnvironmentConfig, len(environments))
	for i, environment := range environments {
		environment.Profiles.Use = append([]string(nil), environment.Profiles.Use...)
		cloned[i] = environment
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
	if strings.TrimSpace(configPath) == "" {
		return
	}
	configDir := filepath.Dir(configPath)
	if configDir == "." || configDir == "" {
		return
	}
	if !filepath.IsAbs(c.Node.StateDir) {
		c.Node.StateDir = filepath.Clean(filepath.Join(configDir, c.Node.StateDir))
	}
	c.Runtime.Scripts.Init = resolveConfigScriptPath(configDir, c.Runtime.Scripts.Init)
	c.Runtime.Scripts.Start = resolveConfigScriptPath(configDir, c.Runtime.Scripts.Start)
	c.Runtime.Scripts.Stop = resolveConfigScriptPath(configDir, c.Runtime.Scripts.Stop)
	c.syncRuntimeFields()
}

func resolveConfigScriptPath(configDir, path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path == "builtin" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(configDir, path))
}

// Validate checks whether the script entry points are usable.
func (c ScriptsConfig) Validate() error {
	entries := []struct {
		name  string
		value string
	}{
		{name: "runtime.scripts.init", value: c.Init},
		{name: "runtime.scripts.start", value: c.Start},
		{name: "runtime.scripts.stop", value: c.Stop},
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
		return fmt.Errorf("runtime.scripts.init, runtime.scripts.start, and runtime.scripts.stop must all be builtin or all be local file paths")
	}
	return nil
}
