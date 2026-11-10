// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	dockerunits "github.com/docker/go-units"
)

// CacheUpstreamID returns the stable route component used for one cache
// upstream. The complete validated host is hashed so hosts that differ only
// by port remain distinct while the value stays valid in an OCI repository
// path.
func CacheUpstreamID(host string) string {
	digest := sha256.Sum256([]byte(host))
	return hex.EncodeToString(digest[:])
}

var gatewayDNSLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Duration stores one configuration duration value.
type Duration time.Duration

// MarshalJSON encodes a duration with Go duration syntax.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON decodes a duration written with Go duration syntax.
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
	Listen            string   `json:"listen"`
	PublicURL         string   `json:"public_url"`
	TrustedProxyCIDRs []string `json:"trusted_proxy_cidrs,omitempty"`
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

// Validate checks whether the Gateway can start and expose stable public addresses.
func (c GatewayConfig) Validate() error {
	if err := c.HTTP.validate(); err != nil {
		return err
	}
	if err := c.SSH.validate(); err != nil {
		return err
	}
	if err := c.Sessions.validate(); err != nil {
		return err
	}
	return c.Limits.validate()
}

func (c GatewayHTTPConfig) validate() error {
	if err := validateListenAddress(c.Listen); err != nil {
		return fmt.Errorf("gateway.http.listen: %w", err)
	}
	if _, err := normalizeGatewayURL(c.PublicURL); err != nil {
		return fmt.Errorf("gateway.http.public_url: %w", err)
	}
	if len(c.TrustedProxyCIDRs) > 64 {
		return fmt.Errorf("gateway.http.trusted_proxy_cidrs: at most 64 entries are allowed")
	}
	for _, prefix := range c.TrustedProxyCIDRs {
		if _, err := netip.ParsePrefix(prefix); err != nil {
			return fmt.Errorf("gateway.http.trusted_proxy_cidrs: %q is invalid", prefix)
		}
	}
	return nil
}

func (c GatewaySSHConfig) validate() error {
	if err := validateListenAddress(c.Listen); err != nil {
		return fmt.Errorf("gateway.ssh.listen: %w", err)
	}
	if _, err := normalizeGatewaySSHAddress(c.PublicAddr); err != nil {
		return fmt.Errorf("gateway.ssh.public_addr: %w", err)
	}
	if c.MaxChannelsPerConnection < 1 || c.MaxChannelsPerConnection > 1024 {
		return fmt.Errorf("gateway.ssh.max_channels_per_connection must be between 1 and 1024")
	}
	if timeout := c.HandshakeTimeout.ToStdlib(); timeout < time.Second || timeout > time.Minute {
		return fmt.Errorf("gateway.ssh.handshake_timeout must be between 1s and 1m")
	}
	if c.Auth.MaxAttemptsPerIP < 1 || c.Auth.MaxAttemptsPerCodespace < 1 || c.Auth.MaxAttemptsPerIPCodespace < 1 || c.Auth.MaxAttemptsPerPublicKey < 1 {
		return fmt.Errorf("gateway.ssh.auth limits must be positive")
	}
	if c.Auth.FailureWindow.ToStdlib() < time.Minute {
		return fmt.Errorf("gateway.ssh.auth.failure_window must be at least 1m")
	}
	return nil
}

func (c GatewaySessionConfig) validate() error {
	if c.IdleTimeout.ToStdlib() < time.Second {
		return fmt.Errorf("gateway.sessions.idle_timeout must be at least 1s")
	}
	if c.TTL.ToStdlib() < time.Minute {
		return fmt.Errorf("gateway.sessions.ttl must be at least 1m")
	}
	if interval := c.RevalidateInterval.ToStdlib(); interval < time.Second || interval > time.Hour {
		return fmt.Errorf("gateway.sessions.revalidate_interval must be between 1s and 1h")
	}
	if c.MaxPerCodespace < 1 || c.MaxPerCodespace > 10_000 || c.MaxPerUser < 1 || c.MaxPerUser > 10_000 {
		return fmt.Errorf("gateway session limits must be between 1 and 10000")
	}
	return nil
}

func (c GatewayLimitsConfig) validate() error {
	if c.MaxInflightTotal < 1 || c.MaxInflightTotal > 1_000_000 {
		return fmt.Errorf("gateway.limits.max_inflight_total must be between 1 and 1000000")
	}
	if c.MaxInflightPerSession < 1 || c.MaxInflightPerSession > 1024 || c.MaxInflightPerSession > c.MaxInflightTotal {
		return fmt.Errorf("gateway.limits.max_inflight_per_session must be between 1 and 1024 and not exceed the total")
	}
	if c.PublicMaxConnectionsPerEndpoint < 1 || c.PublicMaxConnectionsPerEndpoint > 10_000 || c.PublicMaxConnectionsPerIP < 1 || c.PublicMaxConnectionsPerIP > c.PublicMaxConnectionsPerEndpoint {
		return fmt.Errorf("gateway public connection limits are invalid")
	}
	if c.ValidationMaxInflight < 1 || c.ValidationMaxInflight > 4096 {
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
	if registry.MaxAge.ToStdlib() <= 0 || registry.GCInterval.ToStdlib() <= 0 {
		return fmt.Errorf("cache retention durations must be positive")
	}
	for host, upstream := range registry.Upstreams {
		if err := ValidateCacheUpstream(host, upstream.Allow); err != nil {
			return fmt.Errorf("cache.upstreams: %w", err)
		}
	}
	return nil
}

// ValidateCacheUpstream checks one mirror host and its repository allow patterns.
func ValidateCacheUpstream(host string, allow []string) error {
	if host != strings.ToLower(strings.TrimSpace(host)) || strings.Contains(host, "://") {
		return fmt.Errorf("registry %q must be a lowercase registry host", host)
	}
	parsedRegistry, err := url.Parse("https://" + host)
	if err != nil || parsedRegistry.Host != host || parsedRegistry.Path != "" || strings.Trim(parsedRegistry.Hostname(), ".") == "" {
		return fmt.Errorf("registry %q is invalid", host)
	}
	if len(allow) == 0 {
		return fmt.Errorf("registry %q requires at least one repository pattern", host)
	}
	for _, pattern := range allow {
		trimmed := strings.TrimSpace(pattern)
		if pattern != trimmed || pattern == "" || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "..") {
			return fmt.Errorf("registry %q contains invalid pattern %q", host, pattern)
		}
		if strings.Count(pattern, "*") > 1 || strings.Contains(pattern, "*") && !strings.HasSuffix(pattern, "*") {
			return fmt.Errorf("registry %q pattern %q must only use a trailing wildcard", host, pattern)
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
