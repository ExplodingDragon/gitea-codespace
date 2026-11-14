// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package webide

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const ProviderProtocolVersion = 1

// Manifest identifies the provider bundle mounted from its OCI image.
type Manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Provider      string `json:"provider"`
	Architecture  string `json:"architecture"`
	Version       string `json:"version"`
}

// Prepare validates the mounted provider and creates its persistent data
// directory before Docker binds it into the Dev Container.
func Prepare(provider string) error {
	if err := validateBundle(ProgramRootPath, provider, runtime.GOARCH); err != nil {
		return err
	}
	dataPath := ProviderDataPath(provider)
	if err := os.MkdirAll(dataPath, 0o700); err != nil {
		return fmt.Errorf("create Web IDE data directory: %w", err)
	}
	return nil
}

func validateBundle(root, provider, architecture string) error {
	data, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return fmt.Errorf("read Web IDE provider manifest: %w", err)
	}
	if len(data) > 64*1024 {
		return fmt.Errorf("web IDE provider manifest exceeds 64 KiB")
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("decode Web IDE provider manifest: %w", err)
	}
	if manifest.SchemaVersion != ProviderProtocolVersion {
		return fmt.Errorf("web IDE provider protocol %d is unsupported", manifest.SchemaVersion)
	}
	if manifest.Provider != provider {
		return fmt.Errorf("web IDE provider bundle contains %q instead of %q", manifest.Provider, provider)
	}
	if manifest.Architecture != architecture {
		return fmt.Errorf("web IDE provider architecture %q does not match %q", manifest.Architecture, architecture)
	}
	if manifest.Version == "" {
		return fmt.Errorf("web IDE provider version is required")
	}
	for _, path := range []string{PrepareProgram, ServeProgram} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			return fmt.Errorf("validate Web IDE provider %s: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("web IDE provider %s is not an executable regular file", path)
		}
	}
	return nil
}

// ProviderDataPath returns the persistent outer Runtime path for a provider.
func ProviderDataPath(provider string) string {
	return filepath.Join(PersistentRootPath, provider)
}
