// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/docker/cli/cli/config/configfile"
	configtypes "github.com/docker/cli/cli/config/types"
)

const runtimeDockerConfigDirectory = runtimeRunDirectory + "/docker-config"

// prepareDockerCredentials gives Docker CLI subprocesses the same transient
// registry credentials as the Agent process.
func prepareDockerCredentials(directory string, credentials map[string]*agentv1.RegistryCredential) (func() error, error) {
	if !filepath.IsAbs(directory) {
		return nil, fmt.Errorf("docker configuration directory must be absolute")
	}
	filename := filepath.Join(directory, "config.json")
	cleanup := func() error {
		err := os.Remove(filename)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("remove Docker credentials: %w", err)
		}
		return nil
	}
	if err := cleanup(); err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return cleanup, nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create Docker configuration directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("protect Docker configuration directory: %w", err)
	}

	config := configfile.New(filename)
	for registry, credential := range credentials {
		registry = strings.TrimSpace(registry)
		parsed, err := url.Parse("https://" + registry)
		if err != nil || parsed.Host != registry || parsed.Hostname() == "" || parsed.Path != "" || parsed.User != nil {
			return nil, fmt.Errorf("runtime cache registry address is invalid")
		}
		if credential == nil || strings.TrimSpace(credential.Username) == "" || credential.Password == "" {
			return nil, fmt.Errorf("runtime cache credential is invalid")
		}
		config.AuthConfigs[registry] = configtypes.AuthConfig{
			Username:      strings.TrimSpace(credential.Username),
			Password:      credential.Password,
			ServerAddress: registry,
		}
	}
	if err := config.Save(); err != nil {
		return nil, errors.Join(fmt.Errorf("write Docker credentials: %w", err), cleanup())
	}
	if err := os.Chmod(filename, 0o600); err != nil {
		_ = cleanup()
		return nil, fmt.Errorf("protect Docker credentials: %w", err)
	}
	return cleanup, nil
}
