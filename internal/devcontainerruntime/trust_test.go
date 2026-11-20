// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.dev/codespace/internal/certificates"
	"gitea.dev/codespace/internal/identity"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
)

type trustFile struct {
	header tar.Header
	data   []byte
}

type trustAPIClient struct {
	client.APIClient
	mounts []container.MountPoint
	files  map[string]trustFile
}

func (c *trustAPIClient) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{Mounts: c.mounts}, nil
}

func (c *trustAPIClient) CopyFromContainer(_ context.Context, _, filename string) (io.ReadCloser, container.PathStat, error) {
	file, exists := c.files[filename]
	if !exists {
		return nil, container.PathStat{}, cerrdefs.ErrNotFound
	}
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&file.header); err != nil {
		return nil, container.PathStat{}, err
	}
	if _, err := writer.Write(file.data); err != nil {
		return nil, container.PathStat{}, err
	}
	if err := writer.Close(); err != nil {
		return nil, container.PathStat{}, err
	}
	return io.NopCloser(&buffer), container.PathStat{}, nil
}

func (c *trustAPIClient) CopyToContainer(_ context.Context, _, directory string, content io.Reader, _ container.CopyToContainerOptions) error {
	reader := tar.NewReader(content)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		c.files[path.Join(directory, header.Name)] = trustFile{header: *header, data: data}
	}
}

func TestContainerTrustPreservesNativeCertificatesOnRestart(t *testing.T) {
	t.Parallel()
	roots := make([][]byte, 3)
	for i := range roots {
		authority, err := identity.NewAuthority(time.Now())
		require.NoError(t, err)
		roots[i] = authority.RootPEM()
	}
	caDirectory := t.TempDir()
	const systemPath = "/etc/ssl/certs/ca-certificates.crt"
	const actualPath = "/etc/ca-certificates/extracted/tls-ca-bundle.pem"
	original := append([]byte("# Image enterprise trust\n"), roots[0]...)
	api := &trustAPIClient{
		mounts: []container.MountPoint{{Source: caDirectory, Destination: path.Dir(containerPlatformCAPath)}},
		files: map[string]trustFile{
			systemPath: {header: tar.Header{Name: path.Base(systemPath), Typeflag: tar.TypeSymlink, Linkname: "../../ca-certificates/extracted/tls-ca-bundle.pem", Mode: 0o777}},
			actualPath: {header: tar.Header{Name: path.Base(actualPath), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(original))}, data: original},
		},
	}
	for _, platform := range [][]byte{roots[1], roots[2], roots[0]} {
		require.NoError(t, os.WriteFile(filepath.Join(caDirectory, "bundle.pem"), platform, 0o644))
		require.NoError(t, prepareContainerTrust(t.Context(), api, "primary"))
		combined := api.files[actualPath].data
		require.True(t, bytes.HasPrefix(combined, original))
		parsed, err := certificates.ParseSystemCABundle(string(combined))
		require.NoError(t, err)
		expected, err := certificates.ParseCABundle(string(append(bytes.Clone(roots[0]), platform...)))
		require.NoError(t, err)
		require.Equal(t, expected, parsed)
		require.Equal(t, 1, strings.Count(string(combined), platformCABegin))
		require.Equal(t, int64(0o644), api.files[actualPath].header.Mode)
		require.Equal(t, actualPath, api.files[containerCABundlePath].header.Linkname)
	}
	delete(api.files, actualPath)
	require.ErrorContains(t, prepareContainerTrust(t.Context(), api, "primary"), "install ca-certificates")
	api.mounts = nil
	require.NoError(t, prepareContainerTrust(t.Context(), api, "companion"))
}
