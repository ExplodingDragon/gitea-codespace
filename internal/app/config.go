// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
)

var defaultConfigNames = []string{
	"codespace.yaml",
	"codespace.yml",
}

var errConfigNotFound = errors.New("config file not found")

const defaultRegisterConfigPath = "codespace.yaml"

var (
	gatewayDNSLabelPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	codeServerVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)
	environmentTagPattern    = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
)

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
	Node    NodeConfig    `yaml:"node"`
	Gateway GatewayConfig `yaml:"gateway"`
	Runtime RuntimeConfig `yaml:"runtime"`

	provisionerKind   string
	runtimeExecutable string
}

// NodeConfig stores manager node behavior and local state.
type NodeConfig struct {
	StateDir        string   `yaml:"state_dir"`
	Name            string   `yaml:"name"`
	PollInterval    Duration `yaml:"poll_interval"`
	DeclareInterval Duration `yaml:"declare_interval"`
	CapacityTotal   int32    `yaml:"capacity_total"`
	StartupWorkers  int32    `yaml:"startup_workers"`
	CleanupWorkers  int32    `yaml:"cleanup_workers"`
	HTTPTimeout     Duration `yaml:"http_timeout"`
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
}

// GatewayConfig stores user-facing gateway settings.
type GatewayConfig struct {
	HTTP     GatewayHTTPConfig    `yaml:"http"`
	SSH      GatewaySSHConfig     `yaml:"ssh"`
	Sessions GatewaySessionConfig `yaml:"sessions"`
	Limits   GatewayLimitsConfig  `yaml:"limits"`
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
	HandshakeTimeout         Duration             `yaml:"handshake_timeout"`
	MaxChannelsPerConnection int                  `yaml:"max_channels_per_connection"`
	Auth                     GatewaySSHAuthConfig `yaml:"auth"`
}

// GatewaySSHAuthConfig stores SSH authentication rate limits.
type GatewaySSHAuthConfig struct {
	MaxAttemptsPerIP          int      `yaml:"max_attempts_per_ip_per_minute"`
	MaxAttemptsPerCodespace   int      `yaml:"max_attempts_per_codespace_per_minute"`
	MaxAttemptsPerIPCodespace int      `yaml:"max_attempts_per_ip_codespace_per_minute"`
	MaxAttemptsPerPublicKey   int      `yaml:"max_attempts_per_public_key_per_minute"`
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

// RuntimeConfig stores Incus and runtime environment settings.
type RuntimeConfig struct {
	Git          RuntimeGitConfig    `yaml:"git"`
	WebIDE       RuntimeWebIDEConfig `yaml:"web_ide"`
	Cache        RuntimeCacheConfig  `yaml:"cache"`
	Incus        RuntimeIncusConfig  `yaml:"incus"`
	Environments []EnvironmentConfig `yaml:"environments"`
}

// RuntimeCacheConfig stores the optional OCI mirror and BuildKit registry cache settings.
type RuntimeCacheConfig struct {
	BuildRegistry string            `yaml:"registry"`
	Mirrors       map[string]string `yaml:"mirrors"`
}

// RuntimeWebIDEConfig stores the platform Web IDE version for new environments.
type RuntimeWebIDEConfig struct {
	CodeServerVersion string `yaml:"code_server_version"`
}

// RuntimeGitConfig stores Manager-local Git credential generation settings.
type RuntimeGitConfig struct {
	SSHKeyType string `yaml:"ssh_key_type"`
}

// RuntimeIncusConfig stores Incus connection and namespace settings.
type RuntimeIncusConfig struct {
	Endpoint string                    `yaml:"endpoint"`
	Project  RuntimeIncusProjectConfig `yaml:"project"`
	Storage  RuntimeIncusStorageConfig `yaml:"storage"`
	Network  RuntimeIncusNetworkConfig `yaml:"network"`
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
	Description string                     `yaml:"description"`
	Type        string                     `yaml:"type"`
	Source      EnvironmentSourceConfig    `yaml:"source"`
	Resources   EnvironmentResourcesConfig `yaml:"resources"`
	Profiles    []string                   `yaml:"profiles"`
}

// EnvironmentSourceConfig stores the base used to create one runtime.
type EnvironmentSourceConfig struct {
	Image    string                           `yaml:"image"`
	Instance *EnvironmentInstanceSourceConfig `yaml:"instance"`
}

// EnvironmentInstanceSourceConfig identifies an instance on the configured Incus server.
type EnvironmentInstanceSourceConfig struct {
	Project string `yaml:"project"`
	Name    string `yaml:"name"`
}

// EnvironmentResourcesConfig stores runtime resource limits.
type EnvironmentResourcesConfig struct {
	CPU      int32  `yaml:"cpu"`
	Memory   string `yaml:"memory"`
	RootDisk string `yaml:"root_disk"`
}

// DefaultConfig returns one runnable reference configuration.
func DefaultConfig() Config {
	config := Config{
		Node: NodeConfig{
			StateDir:        "codespace-state",
			Name:            "codespace-manager",
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
				HandshakeTimeout:         Duration(30 * time.Second),
				MaxChannelsPerConnection: 32,
				Auth: GatewaySSHAuthConfig{
					MaxAttemptsPerIP:          30,
					MaxAttemptsPerCodespace:   20,
					MaxAttemptsPerIPCodespace: 10,
					MaxAttemptsPerPublicKey:   30,
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
		},
		Runtime: RuntimeConfig{
			Git: RuntimeGitConfig{
				SSHKeyType: "ed25519",
			},
			WebIDE: RuntimeWebIDEConfig{CodeServerVersion: "4.121.0"},
			Incus: RuntimeIncusConfig{
				Endpoint: "unix:///var/lib/incus/unix.socket",
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
					Tag:  "default",
					Type: "container",
					Source: EnvironmentSourceConfig{
						Image: "images:debian/12",
					},
					Resources: EnvironmentResourcesConfig{
						CPU:      1,
						Memory:   "1GiB",
						RootDisk: "10GiB",
					},
					Profiles: []string{"default"},
				},
			},
		},
	}
	config.provisionerKind = "incus"
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
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat config %s: %w", candidate, err)
		}
	}

	return "", fmt.Errorf("%w, tried %s", errConfigNotFound, strings.Join(defaultConfigNames, ", "))
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
		if strings.TrimSpace(path) != "" || !errors.Is(err, errConfigNotFound) {
			return Config{}, err
		}
		config := DefaultConfig()
		config.applyDefaults()
		config.resolveRelativePaths(defaultRegisterConfigPath)
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
		c.validateEnvironments,
		c.validateRuntimeGit,
		c.validateRuntimeWebIDE,
		c.validateRuntimeCache,
		c.validateRuntimeIncus,
		c.validateNode,
		c.validateGatewayAddresses,
		c.Gateway.Validate,
	} {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) validateRuntimeCache() error {
	if registry := strings.TrimSpace(c.Runtime.Cache.BuildRegistry); registry != "" {
		if err := validateRegistryURL(registry, true); err != nil {
			return fmt.Errorf("runtime.cache.registry: %w", err)
		}
	}
	for registry, mirror := range c.Runtime.Cache.Mirrors {
		if registry != strings.ToLower(strings.TrimSpace(registry)) || strings.Contains(registry, "://") {
			return fmt.Errorf("runtime.cache.mirrors registry %q must be a lowercase registry host", registry)
		}
		parsedRegistry, err := url.Parse("https://" + registry)
		if err != nil || parsedRegistry.Host != registry || parsedRegistry.Path != "" {
			return fmt.Errorf("runtime.cache.mirrors registry %q is invalid", registry)
		}
		if err := validateRegistryURL(strings.TrimSpace(mirror), false); err != nil {
			return fmt.Errorf("runtime.cache.mirrors.%s: %w", registry, err)
		}
	}
	return nil
}

func validateRegistryURL(value string, requirePath bool) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must not contain credentials, query, or fragment")
	}
	if requirePath && strings.Trim(parsed.Path, "/") == "" {
		return fmt.Errorf("must include a cache namespace path")
	}
	repository := parsed.Host
	if namespace := strings.Trim(parsed.Path, "/"); namespace != "" {
		repository += "/" + namespace
	}
	if _, err := reference.WithName(repository); err != nil {
		return fmt.Errorf("must contain a valid OCI registry host and namespace: %w", err)
	}
	return nil
}

func (c Config) validateRuntimeWebIDE() error {
	version := strings.TrimSpace(c.Runtime.WebIDE.CodeServerVersion)
	if !codeServerVersionPattern.MatchString(version) {
		return fmt.Errorf("runtime.web_ide.code_server_version must be an explicit semantic version")
	}
	return nil
}

func (c Config) validateRuntimeIncus() error {
	if c.Runtime.Incus.Network.Manage && !c.Runtime.Incus.Project.Manage {
		return fmt.Errorf("runtime.incus.network.manage requires runtime.incus.project.manage")
	}
	endpoint := strings.TrimSpace(c.Runtime.Incus.Endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "unix" && parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("runtime.incus.endpoint must use unix, http, or https")
	}
	if parsed.Scheme == "unix" && (parsed.Host != "" || !filepath.IsAbs(parsed.Path)) {
		return fmt.Errorf("runtime.incus.endpoint unix path must be absolute")
	}
	if parsed.Scheme != "unix" && parsed.Host == "" {
		return fmt.Errorf("runtime.incus.endpoint host is required")
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

func (c Config) validateEnvironments() error {
	if len(c.Runtime.Environments) == 0 {
		return fmt.Errorf("runtime.environments must define at least one environment")
	}
	if len(c.Runtime.Environments) > 64 {
		return fmt.Errorf("runtime.environments must not exceed 64 environments")
	}
	seen := map[string]struct{}{}
	for _, environment := range c.Runtime.Environments {
		tag := strings.ToLower(strings.TrimSpace(environment.Tag))
		if !environmentTagPattern.MatchString(tag) {
			return fmt.Errorf("runtime.environments tag %q must contain only lowercase letters, digits, underscores, or hyphens", tag)
		}
		if _, ok := seen[tag]; ok {
			return fmt.Errorf("runtime.environments tag %q is duplicated", tag)
		}
		seen[tag] = struct{}{}
		if len(environment.Description) > 255 {
			return fmt.Errorf("runtime.environments.%s.description must not exceed 255 bytes", tag)
		}
		if err := environment.validate(tag); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) applyDefaults() {
	defaults := DefaultConfig()

	c.Node.applyDefaults(defaults.Node)
	c.Gateway.applyDefaults(defaults.Gateway)
	c.Runtime.applyDefaults(defaults.Runtime)
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

func (c Config) validateGatewayAddresses() error {
	if strings.TrimSpace(c.Gateway.HTTP.Listen) == "" {
		return fmt.Errorf("gateway.http.listen is required")
	}
	if strings.TrimSpace(c.Gateway.SSH.Listen) == "" {
		return fmt.Errorf("gateway.ssh.listen is required")
	}
	if _, err := normalizeManagerGatewayURL(c.Gateway.HTTP.PublicURL); err != nil {
		return fmt.Errorf("gateway.http.public_url is invalid: %w", err)
	}
	if _, err := normalizeManagerGatewaySSHAddr(c.Gateway.SSH.PublicAddr); err != nil {
		return fmt.Errorf("gateway.ssh.public_addr is invalid: %w", err)
	}
	return nil
}

func (c Config) validateNode() error {
	if strings.TrimSpace(c.Node.StateDir) == "" {
		return fmt.Errorf("node.state_dir is required")
	}
	if strings.TrimSpace(c.Node.Name) == "" {
		return fmt.Errorf("node.name is required")
	}
	if strings.TrimSpace(c.Gateway.HTTP.PublicURL) == "" {
		return fmt.Errorf("gateway.http.public_url is required")
	}
	if c.Node.CapacityTotal < 1 || c.Node.CapacityTotal > 10000 {
		return fmt.Errorf("node.capacity_total must be between 1 and 10000")
	}
	if c.Node.StartupWorkers < 1 || c.Node.StartupWorkers > 256 {
		return fmt.Errorf("node.startup_workers must be between 1 and 256")
	}
	if c.Node.CleanupWorkers < 1 || c.Node.CleanupWorkers > 256 {
		return fmt.Errorf("node.cleanup_workers must be between 1 and 256")
	}
	return nil
}

func (c EnvironmentConfig) validate(tag string) error {
	switch normalizeEnvironmentType(c.Type) {
	case "container", "virtual-machine":
	default:
		return fmt.Errorf("runtime.environments.%s.type must be lxc or vm", tag)
	}
	hasImage := strings.TrimSpace(c.Source.Image) != ""
	hasInstance := c.Source.Instance != nil
	if hasImage == hasInstance {
		return fmt.Errorf("runtime.environments.%s.source must select exactly one image or instance", tag)
	}
	if hasInstance && strings.TrimSpace(c.Source.Instance.Name) == "" {
		return fmt.Errorf("runtime.environments.%s.source.instance.name is required", tag)
	}
	if c.Resources.CPU < 1 {
		return fmt.Errorf("runtime.environments.%s.resources.cpu must be positive", tag)
	}
	if strings.TrimSpace(c.Resources.Memory) == "" {
		return fmt.Errorf("runtime.environments.%s.resources.memory is required", tag)
	}
	if strings.TrimSpace(c.Resources.RootDisk) == "" {
		return fmt.Errorf("runtime.environments.%s.resources.root_disk is required", tag)
	}
	if len(c.Profiles) == 0 {
		return fmt.Errorf("runtime.environments.%s.profiles is required", tag)
	}
	return nil
}

// Validate checks whether gateway settings are usable.
func (c GatewayConfig) Validate() error {
	if c.Limits.MaxInflightTotal < 1 || c.Limits.MaxInflightTotal > 1000000 {
		return fmt.Errorf("gateway.limits.max_inflight_total must be between 1 and 1000000")
	}
	if c.Limits.MaxInflightPerSession < 1 || c.Limits.MaxInflightPerSession > 1024 {
		return fmt.Errorf("gateway.limits.max_inflight_per_session must be between 1 and 1024")
	}
	if c.Limits.MaxInflightPerSession > c.Limits.MaxInflightTotal {
		return fmt.Errorf("gateway.limits.max_inflight_per_session must not exceed gateway.limits.max_inflight_total")
	}
	if c.SSH.MaxChannelsPerConnection < 1 || c.SSH.MaxChannelsPerConnection > 1024 {
		return fmt.Errorf("gateway.ssh.max_channels_per_connection must be between 1 and 1024")
	}
	if timeout := c.SSH.HandshakeTimeout.ToStdlib(); timeout < time.Second || timeout > time.Minute {
		return fmt.Errorf("gateway.ssh.handshake_timeout must be between 1s and 1m")
	}
	if c.SSH.Auth.MaxAttemptsPerIP < 1 {
		return fmt.Errorf("gateway.ssh.auth.max_attempts_per_ip_per_minute must be at least 1")
	}
	if c.SSH.Auth.MaxAttemptsPerCodespace < 1 {
		return fmt.Errorf("gateway.ssh.auth.max_attempts_per_codespace_per_minute must be at least 1")
	}
	if c.SSH.Auth.MaxAttemptsPerIPCodespace < 1 {
		return fmt.Errorf("gateway.ssh.auth.max_attempts_per_ip_codespace_per_minute must be at least 1")
	}
	if c.SSH.Auth.MaxAttemptsPerPublicKey < 1 {
		return fmt.Errorf("gateway.ssh.auth.max_attempts_per_public_key_per_minute must be at least 1")
	}
	if window := c.SSH.Auth.FailureWindow.ToStdlib(); window < time.Minute {
		return fmt.Errorf("gateway.ssh.auth.failure_window must be at least 1m")
	}
	if timeout := c.Sessions.IdleTimeout.ToStdlib(); timeout < time.Second {
		return fmt.Errorf("gateway.sessions.idle_timeout must be at least 1s")
	}
	if ttl := c.Sessions.TTL.ToStdlib(); ttl < time.Minute {
		return fmt.Errorf("gateway.sessions.ttl must be at least 1m")
	}
	if interval := c.Sessions.RevalidateInterval.ToStdlib(); interval < time.Second || interval > time.Hour {
		return fmt.Errorf("gateway.sessions.revalidate_interval must be between 1s and 1h")
	}
	if c.Sessions.MaxPerCodespace < 1 || c.Sessions.MaxPerCodespace > 10000 {
		return fmt.Errorf("gateway.sessions.max_per_codespace must be between 1 and 10000")
	}
	if c.Sessions.MaxPerUser < 1 || c.Sessions.MaxPerUser > 10000 {
		return fmt.Errorf("gateway.sessions.max_per_user must be between 1 and 10000")
	}
	if c.Limits.PublicMaxConnectionsPerEndpoint < 1 || c.Limits.PublicMaxConnectionsPerEndpoint > 10000 {
		return fmt.Errorf("gateway.limits.public_max_connections_per_endpoint must be between 1 and 10000")
	}
	if c.Limits.PublicMaxConnectionsPerIP < 1 || c.Limits.PublicMaxConnectionsPerIP > 10000 {
		return fmt.Errorf("gateway.limits.public_max_connections_per_ip must be between 1 and 10000")
	}
	if c.Limits.PublicMaxConnectionsPerIP > c.Limits.PublicMaxConnectionsPerEndpoint {
		return fmt.Errorf("gateway.limits.public_max_connections_per_ip must not exceed gateway.limits.public_max_connections_per_endpoint")
	}
	if c.Limits.ValidationMaxInflight < 1 || c.Limits.ValidationMaxInflight > 4096 {
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
	if c.SSH.HandshakeTimeout == 0 {
		c.SSH.HandshakeTimeout = defaults.SSH.HandshakeTimeout
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
	if strings.TrimSpace(c.Git.SSHKeyType) == "" {
		c.Git.SSHKeyType = defaults.Git.SSHKeyType
	}
	if strings.TrimSpace(c.WebIDE.CodeServerVersion) == "" {
		c.WebIDE.CodeServerVersion = defaults.WebIDE.CodeServerVersion
	}
	if strings.TrimSpace(c.Incus.Endpoint) == "" {
		c.Incus.Endpoint = defaults.Incus.Endpoint
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
		c.Environments[i].Tag = strings.ToLower(strings.TrimSpace(c.Environments[i].Tag))
		c.Environments[i].Description = strings.TrimSpace(c.Environments[i].Description)
		c.Environments[i].applyDefaults(defaults.Environments[0])
	}
}

func (c *EnvironmentConfig) applyDefaults(defaults EnvironmentConfig) {
	if strings.TrimSpace(c.Tag) == "" {
		c.Tag = defaults.Tag
	}
	if strings.TrimSpace(c.Type) == "" {
		c.Type = defaults.Type
	}
	if c.Source.Instance == nil && strings.TrimSpace(c.Source.Image) == "" {
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
	if len(c.Profiles) == 0 {
		c.Profiles = append([]string(nil), defaults.Profiles...)
	}
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

func cloneEnvironments(environments []EnvironmentConfig) []EnvironmentConfig {
	cloned := make([]EnvironmentConfig, len(environments))
	for i, environment := range environments {
		environment.Profiles = append([]string(nil), environment.Profiles...)
		if environment.Source.Instance != nil {
			instance := *environment.Source.Instance
			environment.Source.Instance = &instance
		}
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
}
