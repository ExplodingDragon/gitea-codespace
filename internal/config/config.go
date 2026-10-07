// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	dockerunits "github.com/docker/go-units"
)

var gatewayDNSLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Duration stores one configuration duration value.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	value, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*d = Duration(value)
	return nil
}

// ToStdlib returns the stdlib duration.
func (d Duration) ToStdlib() time.Duration {
	return time.Duration(d)
}

// GatewayConfig stores user-facing Gateway settings.
type GatewayConfig struct {
	HTTP     GatewayHTTPConfig    `json:"http"`
	SSH      GatewaySSHConfig     `json:"ssh"`
	Sessions GatewaySessionConfig `json:"sessions"`
	Limits   GatewayLimitsConfig  `json:"limits"`
}

// GatewayHTTPConfig stores the Gateway HTTP listener and public URL.
type GatewayHTTPConfig struct {
	Listen    string `json:"listen"`
	PublicURL string `json:"public_url"`
}

// GatewaySSHConfig stores the Gateway SSH listener, public address, and limits.
type GatewaySSHConfig struct {
	Listen                   string               `json:"listen"`
	PublicAddr               string               `json:"public_addr"`
	HandshakeTimeout         Duration             `json:"handshake_timeout"`
	MaxChannelsPerConnection int                  `json:"max_channels_per_connection"`
	Auth                     GatewaySSHAuthConfig `json:"auth"`
}

// GatewaySSHAuthConfig stores SSH authentication rate limits.
type GatewaySSHAuthConfig struct {
	MaxAttemptsPerIP          int      `json:"max_attempts_per_ip_per_minute"`
	MaxAttemptsPerCodespace   int      `json:"max_attempts_per_codespace_per_minute"`
	MaxAttemptsPerIPCodespace int      `json:"max_attempts_per_ip_codespace_per_minute"`
	MaxAttemptsPerPublicKey   int      `json:"max_attempts_per_public_key_per_minute"`
	FailureWindow             Duration `json:"failure_window"`
}

// GatewaySessionConfig stores browser and SSH session limits.
type GatewaySessionConfig struct {
	TTL                Duration `json:"ttl"`
	IdleTimeout        Duration `json:"idle_timeout"`
	RevalidateInterval Duration `json:"revalidate_interval"`
	MaxPerCodespace    int      `json:"max_per_codespace"`
	MaxPerUser         int      `json:"max_per_user"`
}

// GatewayLimitsConfig stores Gateway request and connection limits.
type GatewayLimitsConfig struct {
	MaxInflightTotal                int `json:"max_inflight_total"`
	MaxInflightPerSession           int `json:"max_inflight_per_session"`
	PublicMaxConnectionsPerEndpoint int `json:"public_max_connections_per_endpoint"`
	PublicMaxConnectionsPerIP       int `json:"public_max_connections_per_ip"`
	ValidationMaxInflight           int `json:"validation_max_inflight"`
}

// RuntimeCacheUpstreamConfig stores repository allow patterns for one upstream registry.
type RuntimeCacheUpstreamConfig struct {
	Allow []string `json:"allow"`
}

// DefaultGatewayConfig returns usable defaults for one independently deployed Gateway.
func DefaultGatewayConfig() GatewayConfig {
	return GatewayConfig{
		HTTP: GatewayHTTPConfig{Listen: ":18081", PublicURL: "http://gateway.example.com:18081"},
		SSH: GatewaySSHConfig{
			Listen: ":2222", PublicAddr: "gateway.example.com:22", HandshakeTimeout: Duration(30 * time.Second), MaxChannelsPerConnection: 32,
			Auth: GatewaySSHAuthConfig{MaxAttemptsPerIP: 30, MaxAttemptsPerCodespace: 20, MaxAttemptsPerIPCodespace: 10, MaxAttemptsPerPublicKey: 30, FailureWindow: Duration(10 * time.Minute)},
		},
		Sessions: GatewaySessionConfig{TTL: Duration(8 * time.Hour), IdleTimeout: Duration(30 * time.Minute), RevalidateInterval: Duration(5 * time.Minute), MaxPerCodespace: 32, MaxPerUser: 128},
		Limits:   GatewayLimitsConfig{MaxInflightTotal: 4096, MaxInflightPerSession: 32, PublicMaxConnectionsPerEndpoint: 64, PublicMaxConnectionsPerIP: 16, ValidationMaxInflight: 128},
	}
}

// Validate checks whether the Gateway can start and expose stable public addresses.
func (c GatewayConfig) Validate() error {
	if err := validateListenAddress(c.HTTP.Listen); err != nil {
		return fmt.Errorf("gateway.http.listen: %w", err)
	}
	if err := validateListenAddress(c.SSH.Listen); err != nil {
		return fmt.Errorf("gateway.ssh.listen: %w", err)
	}
	if _, err := normalizeGatewayURL(c.HTTP.PublicURL); err != nil {
		return fmt.Errorf("gateway.http.public_url: %w", err)
	}
	if _, err := normalizeGatewaySSHAddress(c.SSH.PublicAddr); err != nil {
		return fmt.Errorf("gateway.ssh.public_addr: %w", err)
	}
	if c.Limits.MaxInflightTotal < 1 || c.Limits.MaxInflightTotal > 1_000_000 {
		return fmt.Errorf("gateway.limits.max_inflight_total must be between 1 and 1000000")
	}
	if c.Limits.MaxInflightPerSession < 1 || c.Limits.MaxInflightPerSession > 1024 || c.Limits.MaxInflightPerSession > c.Limits.MaxInflightTotal {
		return fmt.Errorf("gateway.limits.max_inflight_per_session must be between 1 and 1024 and not exceed the total")
	}
	if c.SSH.MaxChannelsPerConnection < 1 || c.SSH.MaxChannelsPerConnection > 1024 {
		return fmt.Errorf("gateway.ssh.max_channels_per_connection must be between 1 and 1024")
	}
	if timeout := c.SSH.HandshakeTimeout.ToStdlib(); timeout < time.Second || timeout > time.Minute {
		return fmt.Errorf("gateway.ssh.handshake_timeout must be between 1s and 1m")
	}
	if c.SSH.Auth.MaxAttemptsPerIP < 1 || c.SSH.Auth.MaxAttemptsPerCodespace < 1 || c.SSH.Auth.MaxAttemptsPerIPCodespace < 1 || c.SSH.Auth.MaxAttemptsPerPublicKey < 1 {
		return fmt.Errorf("gateway.ssh.auth limits must be positive")
	}
	if c.SSH.Auth.FailureWindow.ToStdlib() < time.Minute {
		return fmt.Errorf("gateway.ssh.auth.failure_window must be at least 1m")
	}
	if c.Sessions.IdleTimeout.ToStdlib() < time.Second {
		return fmt.Errorf("gateway.sessions.idle_timeout must be at least 1s")
	}
	if c.Sessions.TTL.ToStdlib() < time.Minute {
		return fmt.Errorf("gateway.sessions.ttl must be at least 1m")
	}
	if interval := c.Sessions.RevalidateInterval.ToStdlib(); interval < time.Second || interval > time.Hour {
		return fmt.Errorf("gateway.sessions.revalidate_interval must be between 1s and 1h")
	}
	if c.Sessions.MaxPerCodespace < 1 || c.Sessions.MaxPerCodespace > 10_000 || c.Sessions.MaxPerUser < 1 || c.Sessions.MaxPerUser > 10_000 {
		return fmt.Errorf("gateway session limits must be between 1 and 10000")
	}
	if c.Limits.PublicMaxConnectionsPerEndpoint < 1 || c.Limits.PublicMaxConnectionsPerEndpoint > 10_000 || c.Limits.PublicMaxConnectionsPerIP < 1 || c.Limits.PublicMaxConnectionsPerIP > c.Limits.PublicMaxConnectionsPerEndpoint {
		return fmt.Errorf("gateway public connection limits are invalid")
	}
	if c.Limits.ValidationMaxInflight < 1 || c.Limits.ValidationMaxInflight > 4096 {
		return fmt.Errorf("gateway.limits.validation_max_inflight must be between 1 and 4096")
	}
	return nil
}

func validateListenAddress(value string) error {
	_, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("must use host:port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func normalizeGatewayURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", fmt.Errorf("must be an absolute HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("must be an origin without credentials, path, query, or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	if err := validateGatewayDNSHost(host); err != nil {
		return "", err
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("port must be between 1 and 65535")
		}
		if parsed.Scheme == "http" && number == 80 || parsed.Scheme == "https" && number == 443 {
			port = ""
		}
	}
	normalized := parsed.Scheme + "://" + host
	if port != "" {
		normalized += ":" + port
	}
	return normalized, nil
}

func normalizeGatewaySSHAddress(value string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("must use host:port")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if err := validateGatewayDNSHost(host); err != nil {
		return "", err
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", fmt.Errorf("port must be between 1 and 65535")
	}
	return net.JoinHostPort(host, strconv.Itoa(number)), nil
}

func validateGatewayDNSHost(host string) error {
	if host == "" || strings.HasSuffix(host, ".") || net.ParseIP(host) != nil || len(host) > 253 {
		return fmt.Errorf("host must be a DNS name without a trailing dot")
	}
	for label := range strings.SplitSeq(host, ".") {
		if !gatewayDNSLabelPattern.MatchString(label) {
			return fmt.Errorf("invalid DNS label %q", label)
		}
	}
	return nil
}

func (registry CacheConfig) validateRegistry() error {
	if err := validateListenAddress(registry.Listen); err != nil {
		return fmt.Errorf("cache.listen: %w", err)
	}
	if err := ValidateRegistryRootURL(strings.TrimRight(strings.TrimSpace(registry.PublicURL), "/")); err != nil {
		return fmt.Errorf("cache.public_url: %w", err)
	}
	if strings.TrimSpace(registry.MaxSize) != "" {
		maxBytes, err := dockerunits.RAMInBytes(registry.MaxSize)
		if err != nil {
			return fmt.Errorf("cache.max_size: %w", err)
		}
		if maxBytes <= 0 {
			return fmt.Errorf("cache.max_size must be positive")
		}
	}
	if registry.Storage.MinFreeSpace != "" {
		n, err := dockerunits.RAMInBytes(registry.Storage.MinFreeSpace)
		if err != nil || n <= 0 {
			return fmt.Errorf("cache min_free_space must be a positive size")
		}
	}
	if registry.MaxAge.ToStdlib() < 0 || registry.GCInterval.ToStdlib() < 0 {
		return fmt.Errorf("cache retention durations must not be negative")
	}
	for host, upstream := range registry.Upstreams {
		if host != strings.ToLower(strings.TrimSpace(host)) || strings.Contains(host, "://") {
			return fmt.Errorf("cache.upstreams registry %q must be a lowercase registry host", host)
		}
		parsedRegistry, err := url.Parse("https://" + host)
		if err != nil || parsedRegistry.Host != host || parsedRegistry.Path != "" || strings.Trim(parsedRegistry.Hostname(), ".") == "" {
			return fmt.Errorf("cache.upstreams registry %q is invalid", host)
		}
		for _, pattern := range upstream.Allow {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "..") {
				return fmt.Errorf("cache.upstreams.%s.allow contains invalid pattern %q", host, pattern)
			}
			if strings.Count(pattern, "*") > 1 || strings.Contains(pattern, "*") && !strings.HasSuffix(pattern, "*") {
				return fmt.Errorf("cache.upstreams.%s.allow pattern %q must only use a trailing wildcard", host, pattern)
			}
		}
	}
	return nil
}

// ValidateRegistryRootURL checks a registry origin used as a Docker mirror.
func ValidateRegistryRootURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("must be an absolute HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must not contain credentials, query, or fragment")
	}
	if strings.Trim(parsed.Path, "/") != "" {
		return fmt.Errorf("must not contain a path because the registry listens at its root")
	}
	return nil
}
