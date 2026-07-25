// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	managerIdentityFormatVersion = 1
	managerIdentityFileName      = "identity.json"
	currentProtocolVersion       = 1
)

// ManagerIdentity stores the registered Manager identity.
type ManagerIdentity struct {
	StateFormatVersion int    `json:"state_format_version"`
	ProtocolVersion    int32  `json:"protocol_version"`
	GiteaURL           string `json:"gitea_url"`
	ManagerID          int64  `json:"manager_id"`
	RegisteredUnix     int64  `json:"registered_unix"`
}

// LoadManagerIdentity loads the registered Manager identity from stateDir.
func LoadManagerIdentity(stateDir string) (ManagerIdentity, error) {
	path, err := managerIdentityPath(stateDir)
	if err != nil {
		return ManagerIdentity{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return ManagerIdentity{}, fmt.Errorf("read manager identity %s: %w", path, err)
	}
	var identity ManagerIdentity
	if err := json.Unmarshal(content, &identity); err != nil {
		return ManagerIdentity{}, fmt.Errorf("decode manager identity %s: %w", path, err)
	}
	if err := identity.Validate(); err != nil {
		return ManagerIdentity{}, fmt.Errorf("validate manager identity %s: %w", path, err)
	}
	return identity, nil
}

// SaveManagerIdentity stores the registered Manager identity in stateDir.
func SaveManagerIdentity(stateDir string, identity ManagerIdentity) error {
	path, err := managerIdentityPath(stateDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("manager identity already exists in %s", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat manager identity %s: %w", path, err)
	}
	identity.StateFormatVersion = managerIdentityFormatVersion
	identity.ProtocolVersion = currentProtocolVersion
	identity.GiteaURL = strings.TrimRight(strings.TrimSpace(identity.GiteaURL), "/")
	if err := identity.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", filepath.Dir(path), err)
	}
	return writeJSONFileAtomic(path, identity)
}

// Validate checks whether the identity file is usable.
func (i ManagerIdentity) Validate() error {
	if i.StateFormatVersion != managerIdentityFormatVersion {
		return fmt.Errorf("state_format_version must be %d", managerIdentityFormatVersion)
	}
	if i.ProtocolVersion != currentProtocolVersion {
		return fmt.Errorf("protocol_version must be %d", currentProtocolVersion)
	}
	parsed, err := url.Parse(strings.TrimSpace(i.GiteaURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("gitea_url must be an absolute URL")
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("gitea_url scheme must be http or https")
	}
	if i.ManagerID <= 0 {
		return fmt.Errorf("manager_id is required")
	}
	if i.RegisteredUnix <= 0 {
		return fmt.Errorf("registered_unix is required")
	}
	return nil
}

func managerIdentityPath(stateDir string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return "", fmt.Errorf("manager.state_dir is required")
	}
	return filepath.Join(stateDir, managerIdentityFileName), nil
}
