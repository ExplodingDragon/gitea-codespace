// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	managerCredentialsFormatVersion = 1
	managerCredentialsFileName      = "manager-credentials.json"
)

// ManagerCredentials stores the local ManagerService identity.
type ManagerCredentials struct {
	FormatVersion int    `json:"format_version"`
	ManagerID     int64  `json:"manager_id"`
	ManagerSecret string `json:"manager_secret"`
}

// LoadManagerCredentials loads the ManagerService identity from stateDir.
func LoadManagerCredentials(stateDir string) (ManagerCredentials, error) {
	path, err := managerCredentialsPath(stateDir)
	if err != nil {
		return ManagerCredentials{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return ManagerCredentials{}, fmt.Errorf("read manager credentials %s: %w", path, err)
	}
	var credentials ManagerCredentials
	if err := json.Unmarshal(content, &credentials); err != nil {
		return ManagerCredentials{}, fmt.Errorf("decode manager credentials %s: %w", path, err)
	}
	if err := credentials.Validate(); err != nil {
		return ManagerCredentials{}, fmt.Errorf("validate manager credentials %s: %w", path, err)
	}
	return credentials, nil
}

// SaveManagerCredentials stores the ManagerService identity in stateDir.
func SaveManagerCredentials(stateDir string, credentials ManagerCredentials) error {
	path, err := managerCredentialsPath(stateDir)
	if err != nil {
		return err
	}
	credentials.FormatVersion = managerCredentialsFormatVersion
	if err := credentials.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", filepath.Dir(path), err)
	}
	return writeJSONFileAtomic(path, credentials)
}

// Validate checks whether the credential file is usable.
func (c ManagerCredentials) Validate() error {
	if c.FormatVersion != managerCredentialsFormatVersion {
		return fmt.Errorf("format_version must be %d", managerCredentialsFormatVersion)
	}
	if c.ManagerID <= 0 {
		return fmt.Errorf("manager_id is required")
	}
	if strings.TrimSpace(c.ManagerSecret) == "" {
		return fmt.Errorf("manager_secret is required")
	}
	return nil
}

func managerCredentialsPath(stateDir string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return "", fmt.Errorf("manager.state_dir is required")
	}
	return filepath.Join(stateDir, managerCredentialsFileName), nil
}
