// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package webide

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateBundle(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, PrepareProgram), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ServeProgram), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ManifestName), []byte(`{"schemaVersion":1,"provider":"example","architecture":"amd64","version":"1.0.0"}`), 0o644))
	require.NoError(t, validateBundle(root, "example", "amd64"))
	require.ErrorContains(t, validateBundle(root, "other", "amd64"), `contains "example" instead of "other"`)
	require.ErrorContains(t, validateBundle(root, "example", "arm64"), `does not match "arm64"`)
}
