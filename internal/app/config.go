// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
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
	Provisioner ProvisionerConfig `json:"provisioner" yaml:"provisioner"`
}

// ServerConfig stores listener and public URL settings.
type ServerConfig struct {
	ListenAddr      string   `json:"listen_addr" yaml:"listen_addr"`
	PublicBaseURL   string   `json:"public_base_url" yaml:"public_base_url"`
	ShutdownTimeout Duration `json:"shutdown_timeout" yaml:"shutdown_timeout"`
}

// GiteaConfig stores the remote Gitea control-plane endpoint.
type GiteaConfig struct {
	URL string `json:"url" yaml:"url"`
}

// GatewayConfig stores user-facing gateway settings.
type GatewayConfig struct {
	SSHHost string `json:"ssh_host" yaml:"ssh_host"`
	SSHPort int    `json:"ssh_port" yaml:"ssh_port"`
}

// ManagerConfig stores embedded manager behavior and capabilities.
type ManagerConfig struct {
	ID                     int64                  `json:"id" yaml:"id"`
	UUID                   string                 `json:"uuid" yaml:"uuid"`
	Token                  string                 `json:"token" yaml:"token"`
	Name                   string                 `json:"name" yaml:"name"`
	GatewayURL             string                 `json:"gateway_url" yaml:"gateway_url"`
	Version                string                 `json:"version" yaml:"version"`
	PollInterval           Duration               `json:"poll_interval" yaml:"poll_interval"`
	PingInterval           Duration               `json:"ping_interval" yaml:"ping_interval"`
	FetchCapacity          int32                  `json:"fetch_capacity" yaml:"fetch_capacity"`
	HTTPTimeout            Duration               `json:"http_timeout" yaml:"http_timeout"`
	Labels                 []string               `json:"labels" yaml:"labels"`
	MaxConcurrency         int32                  `json:"max_concurrency" yaml:"max_concurrency"`
	SupportedInstanceTypes []string               `json:"supported_instance_types" yaml:"supported_instance_types"`
	Images                 []string               `json:"images" yaml:"images"`
	ResourcePresets        []ResourcePresetConfig `json:"resource_presets" yaml:"resource_presets"`
	Features               ManagerFeaturesConfig  `json:"features" yaml:"features"`
	DefaultInitScript      string                 `json:"default_init_script" yaml:"default_init_script"`
}

// ResourcePresetConfig stores one advertised resource preset.
type ResourcePresetConfig struct {
	Name   string `json:"name" yaml:"name"`
	CPU    string `json:"cpu" yaml:"cpu"`
	Memory string `json:"memory" yaml:"memory"`
	Disk   string `json:"disk" yaml:"disk"`
}

// ManagerFeaturesConfig stores manager feature flags.
type ManagerFeaturesConfig struct {
	Web         bool `json:"web" yaml:"web"`
	SSH         bool `json:"ssh" yaml:"ssh"`
	PortPreview bool `json:"port_preview" yaml:"port_preview"`
	PublicPort  bool `json:"public_port" yaml:"public_port"`
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
	Remote     string `json:"remote" yaml:"remote"`
	UnixSocket string `json:"unix_socket" yaml:"unix_socket"`
	Project    string `json:"project" yaml:"project"`
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
			ListenAddr:      ":18080",
			PublicBaseURL:   "http://127.0.0.1:18080",
			ShutdownTimeout: Duration(10 * time.Second),
		},
		Gitea: GiteaConfig{
			URL: "http://127.0.0.1:3000",
		},
		Gateway: GatewayConfig{
			SSHHost: "gateway.example.com",
			SSHPort: 22,
		},
		Manager: ManagerConfig{
			Name:                   "embedded-reference-manager",
			Version:                "0.1.0",
			PollInterval:           Duration(750 * time.Millisecond),
			PingInterval:           Duration(5 * time.Second),
			FetchCapacity:          4,
			HTTPTimeout:            Duration(15 * time.Second),
			Labels:                 []string{"linux", "reference", "embedded"},
			MaxConcurrency:         4,
			SupportedInstanceTypes: []string{"container", "vm"},
			Images:                 []string{"images:debian/12", "images:ubuntu/24.04"},
			ResourcePresets: []ResourcePresetConfig{
				{
					Name:   "small",
					CPU:    "2",
					Memory: "4GiB",
					Disk:   "40GiB",
				},
			},
			Features: ManagerFeaturesConfig{
				Web:         true,
				SSH:         true,
				PortPreview: true,
				PublicPort:  false,
			},
			DefaultInitScript: "npx --yes @devcontainers/cli up --workspace-folder .",
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
		return config, nil
	}
	config, err := decodeConfigFile(configPath)
	if err != nil {
		return Config{}, err
	}
	config.applyDefaults()
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
	if strings.TrimSpace(c.Server.ListenAddr) == "" {
		return fmt.Errorf("server.listen_addr is required")
	}
	if strings.TrimSpace(c.Server.PublicBaseURL) == "" {
		return fmt.Errorf("server.public_base_url is required")
	}
	if strings.TrimSpace(c.Gitea.URL) == "" {
		return fmt.Errorf("gitea.url is required")
	}
	if strings.TrimSpace(c.Manager.UUID) == "" {
		return fmt.Errorf("manager.uuid is required; run register first")
	}
	if strings.TrimSpace(c.Manager.Token) == "" {
		return fmt.Errorf("manager.token is required; run register first")
	}
	if strings.TrimSpace(c.Manager.Name) == "" {
		return fmt.Errorf("manager.name is required")
	}
	if strings.TrimSpace(c.Manager.GatewayURL) == "" {
		return fmt.Errorf("manager.gateway_url is required")
	}
	if strings.TrimSpace(c.Provisioner.Kind) == "" {
		return fmt.Errorf("provisioner.kind is required")
	}
	return nil
}

func (c *Config) applyDefaults() {
	defaults := DefaultConfig()

	if strings.TrimSpace(c.Server.ListenAddr) == "" {
		c.Server.ListenAddr = defaults.Server.ListenAddr
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
	if c.Manager.PingInterval == 0 {
		c.Manager.PingInterval = defaults.Manager.PingInterval
	}
	if c.Manager.FetchCapacity == 0 {
		c.Manager.FetchCapacity = defaults.Manager.FetchCapacity
	}
	if c.Manager.HTTPTimeout == 0 {
		c.Manager.HTTPTimeout = defaults.Manager.HTTPTimeout
	}
	if len(c.Manager.Labels) == 0 {
		c.Manager.Labels = append([]string(nil), defaults.Manager.Labels...)
	}
	if c.Manager.MaxConcurrency == 0 {
		c.Manager.MaxConcurrency = defaults.Manager.MaxConcurrency
	}
	if len(c.Manager.SupportedInstanceTypes) == 0 {
		c.Manager.SupportedInstanceTypes = append([]string(nil), defaults.Manager.SupportedInstanceTypes...)
	}
	if len(c.Manager.Images) == 0 {
		c.Manager.Images = append([]string(nil), defaults.Manager.Images...)
	}
	if len(c.Manager.ResourcePresets) == 0 {
		c.Manager.ResourcePresets = append([]ResourcePresetConfig(nil), defaults.Manager.ResourcePresets...)
	}
	if c.Manager.Features == (ManagerFeaturesConfig{}) {
		c.Manager.Features = defaults.Manager.Features
	}
	if strings.TrimSpace(c.Manager.DefaultInitScript) == "" {
		c.Manager.DefaultInitScript = defaults.Manager.DefaultInitScript
	}
	if strings.TrimSpace(c.Provisioner.Kind) == "" {
		c.Provisioner.Kind = defaults.Provisioner.Kind
	}
	if strings.TrimSpace(c.Provisioner.CodespaceRoot) == "" {
		c.Provisioner.CodespaceRoot = defaults.Provisioner.CodespaceRoot
	}
	if strings.TrimSpace(c.Provisioner.Bootstrap.Shell) == "" {
		c.Provisioner.Bootstrap.Shell = defaults.Provisioner.Bootstrap.Shell
	}
	if strings.TrimSpace(c.Provisioner.Bootstrap.HomeDir) == "" {
		c.Provisioner.Bootstrap.HomeDir = defaults.Provisioner.Bootstrap.HomeDir
	}
}

func buildCapabilities(config Config) *codespacev1.ManagerCapabilities {
	resourcePresets := make([]*codespacev1.ResourcePreset, 0, len(config.Manager.ResourcePresets))
	for _, preset := range config.Manager.ResourcePresets {
		resourcePresets = append(resourcePresets, &codespacev1.ResourcePreset{
			Name:   preset.Name,
			Cpu:    preset.CPU,
			Memory: preset.Memory,
			Disk:   preset.Disk,
		})
	}

	return &codespacev1.ManagerCapabilities{
		GatewayUrl:             config.Manager.GatewayURL,
		Version:                config.Manager.Version,
		Labels:                 append([]string(nil), config.Manager.Labels...),
		MaxConcurrency:         config.Manager.MaxConcurrency,
		CurrentConcurrency:     0,
		SupportedInstanceTypes: append([]string(nil), config.Manager.SupportedInstanceTypes...),
		Images:                 append([]string(nil), config.Manager.Images...),
		ResourcePresets:        resourcePresets,
		Features: &codespacev1.ManagerFeatures{
			Web:         config.Manager.Features.Web,
			Ssh:         config.Manager.Features.SSH,
			PortPreview: config.Manager.Features.PortPreview,
			PublicPort:  config.Manager.Features.PublicPort,
		},
		DefaultInitScript: config.Manager.DefaultInitScript,
	}
}
