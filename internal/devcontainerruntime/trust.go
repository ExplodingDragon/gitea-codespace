// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"gitea.dev/codespace/internal/certificates"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

const (
	platformCABegin = "# BEGIN GITEA CODESPACE CA\n"
	platformCAEnd   = "# END GITEA CODESPACE CA\n"
)

// prepareContainerTrust runs against the stopped container, so systemd and
// image entrypoints see the combined trust before their first HTTPS request.
func prepareContainerTrust(ctx context.Context, api client.APIClient, id string) error {
	inspect, err := api.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}
	var platformBundle []byte
	for _, mount := range inspect.Mounts {
		if mount.Destination == path.Dir(containerPlatformCAPath) {
			platformBundle, err = os.ReadFile(path.Join(mount.Source, "bundle.pem"))
			if err != nil {
				return fmt.Errorf("read platform CA bundle: %w", err)
			}
			break
		}
	}
	if platformBundle == nil {
		return nil // Compose companion services have no platform CA mount.
	}
	// These are the system bundles used by the supported Linux distributions.
	// Follow symlinks so updating trust preserves the distribution's file layout.
	for _, candidate := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	} {
		original, header, err := readContainerCABundle(ctx, api, id, candidate)
		if cerrdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read Dev Container system CA bundle: %w", err)
		}
		bundle, err := mergeContainerCABundle(original, platformBundle)
		if err != nil {
			return err
		}
		var archive bytes.Buffer
		writer := tar.NewWriter(&archive)
		header.Size = int64(len(bundle))
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if _, err := writer.Write(bundle); err != nil {
			return err
		}
		for _, entry := range []*tar.Header{
			{Name: "var/lib/gitea-codespace/trust", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: strings.TrimPrefix(containerCABundlePath, "/"), Typeflag: tar.TypeSymlink, Linkname: "/" + header.Name, Mode: 0o777},
		} {
			if err := writer.WriteHeader(entry); err != nil {
				return err
			}
		}
		if err := writer.Close(); err != nil {
			return err
		}
		// Preserve archive ownership; CopyUIDGID would chown CA files to the
		// container's configured user instead of retaining system ownership.
		if err := api.CopyToContainer(ctx, id, "/", &archive, container.CopyToContainerOptions{}); err != nil {
			return fmt.Errorf("install Dev Container CA bundle before start: %w", err)
		}
		return nil
	}
	return fmt.Errorf("dev container has no supported system CA bundle; install ca-certificates in the base image")
}

func readContainerCABundle(ctx context.Context, api client.APIClient, id, filename string) ([]byte, *tar.Header, error) {
	for range 40 { // Match Linux's symlink traversal limit.
		stream, _, err := api.CopyFromContainer(ctx, id, filename)
		if err != nil {
			return nil, nil, err
		}
		reader := tar.NewReader(stream)
		header, err := reader.Next()
		if err != nil {
			_ = stream.Close()
			return nil, nil, err
		}
		if header.Typeflag == tar.TypeSymlink {
			_ = stream.Close()
			if path.IsAbs(header.Linkname) {
				filename = path.Clean(header.Linkname)
			} else {
				filename = path.Join(path.Dir(filename), header.Linkname)
			}
			continue
		}
		if !header.FileInfo().Mode().IsRegular() || header.Size > 4*1024*1024 {
			_ = stream.Close()
			return nil, nil, fmt.Errorf("system CA bundle %s must be a regular file no larger than 4 MiB", filename)
		}
		content, err := io.ReadAll(reader)
		_ = stream.Close()
		header.Name = strings.TrimPrefix(filename, "/")
		return content, header, err
	}
	return nil, nil, fmt.Errorf("too many symlinks in system CA bundle path %s", filename)
}

func mergeContainerCABundle(original, platform []byte) ([]byte, error) {
	// Keep the image/user's certificates byte-for-byte outside our own block.
	// Replacing that block avoids retaining withdrawn platform certificates.
	if before, rest, found := bytes.Cut(original, []byte(platformCABegin)); found {
		_, after, closed := bytes.Cut(rest, []byte(platformCAEnd))
		if !closed {
			return nil, fmt.Errorf("dev container platform CA block is incomplete")
		}
		original = append(bytes.Clone(before), after...)
	}
	native, err := certificates.ParseSystemCABundle(string(original))
	if err != nil {
		return nil, fmt.Errorf("parse Dev Container system CA bundle: %w", err)
	}
	additional, err := certificates.ParseCABundle(string(platform))
	if err != nil {
		return nil, fmt.Errorf("parse platform CA bundle: %w", err)
	}
	var combined bytes.Buffer
	combined.Write(bytes.TrimRight(original, "\r\n"))
	combined.WriteByte('\n')
	combined.WriteString(platformCABegin)
	for _, fingerprint := range slices.Sorted(maps.Keys(additional)) {
		if _, exists := native[fingerprint]; !exists {
			combined.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: additional[fingerprint].Raw}))
		}
	}
	combined.WriteString(platformCAEnd)
	return combined.Bytes(), nil
}
