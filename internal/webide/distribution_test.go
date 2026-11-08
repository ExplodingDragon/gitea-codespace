// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package webide

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractStandalone(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "code-server.tar.gz")
	writeArchive(t, archive, []tar.Header{
		{Name: "code-server/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "code-server/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "code-server/bin/code-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: 6},
	}, [][]byte{nil, nil, []byte("editor")})
	destination := t.TempDir()
	require.NoError(t, extractStandalone(archive, destination))
	data, err := os.ReadFile(filepath.Join(destination, "bin", "code-server"))
	require.NoError(t, err)
	require.Equal(t, "editor", string(data))
}

func TestExtractStandaloneRejectsEscapingLink(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "code-server.tar.gz")
	writeArchive(t, archive, []tar.Header{
		{Name: "code-server/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "code-server/bin", Typeflag: tar.TypeSymlink, Linkname: "../../outside"},
	}, [][]byte{nil, nil})
	require.ErrorContains(t, extractStandalone(archive, t.TempDir()), "escaping symbolic link")
}

func TestMetadataMatchesSourceAndArchitecture(t *testing.T) {
	program := t.TempDir()
	expected := sourceMetadata{Version: 1, Fingerprint: "source", Architecture: "amd64"}
	data, err := json.Marshal(expected)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(program, ".source.json"), data, 0o600))
	require.True(t, metadataMatches(program, expected))
	require.False(t, metadataMatches(program, sourceMetadata{Version: 1, Fingerprint: "source", Architecture: "arm64"}))
}

func writeArchive(t *testing.T, path string, headers []tar.Header, contents [][]byte) {
	t.Helper()
	file, err := os.Create(path)
	require.NoError(t, err)
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	for index := range headers {
		require.NoError(t, archive.WriteHeader(&headers[index]))
		if len(contents[index]) > 0 {
			_, err = archive.Write(contents[index])
			require.NoError(t, err)
		}
	}
	require.NoError(t, archive.Close())
	require.NoError(t, compressed.Close())
	require.NoError(t, file.Close())
}
