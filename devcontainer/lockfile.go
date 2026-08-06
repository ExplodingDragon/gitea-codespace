// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Lockfile records the immutable OCI resolution of repository Features.
type Lockfile struct {
	Features map[string]LockedFeature `json:"features"`
}

// LockedFeature identifies one resolved Feature and its dependencies.
type LockedFeature struct {
	Version   string   `json:"version"`
	Resolved  string   `json:"resolved"`
	Integrity string   `json:"integrity"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// LockfilePath returns the standard lockfile path for a configuration file.
func LockfilePath(configurationPath string) string {
	name := "devcontainer-lock.json"
	if strings.HasPrefix(filepath.Base(configurationPath), ".") {
		name = ".devcontainer-lock.json"
	}
	return filepath.Join(filepath.Dir(configurationPath), name)
}

// ReadLockfile reads the lockfile next to a configuration file.
func ReadLockfile(configurationPath string) (Lockfile, bool, error) {
	content, err := os.ReadFile(LockfilePath(configurationPath))
	if errors.Is(err, os.ErrNotExist) {
		return Lockfile{}, false, nil
	}
	if err != nil {
		return Lockfile{}, false, err
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return Lockfile{}, false, nil
	}
	var lockfile Lockfile
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lockfile); err != nil {
		return Lockfile{}, false, fmt.Errorf("decode Dev Container lockfile: %w", err)
	}
	if lockfile.Features == nil {
		lockfile.Features = map[string]LockedFeature{}
	}
	return lockfile, true, nil
}

// WriteLockfile atomically updates a lockfile or validates it when frozen is true.
func WriteLockfile(configurationPath string, lockfile Lockfile, frozen bool) error {
	content, err := json.MarshalIndent(lockfile, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	path := LockfilePath(configurationPath)
	existing, readErr := os.ReadFile(path)
	if readErr == nil {
		var existingValue, generatedValue any
		if json.Unmarshal(existing, &existingValue) == nil && json.Unmarshal(content, &generatedValue) == nil {
			existing, _ = json.Marshal(existingValue)
			contentForComparison, _ := json.Marshal(generatedValue)
			if bytes.Equal(existing, contentForComparison) {
				return nil
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if frozen {
		if errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("dev container lockfile does not exist")
		}
		return fmt.Errorf("dev container lockfile does not match resolved Features")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".devcontainer-lock-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
