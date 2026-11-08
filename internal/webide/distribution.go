// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package webide

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	maxArchiveBytes   = 512 << 20
	maxExtractedBytes = 2 << 30
	maxArchiveEntries = 200_000
	platformCABundle  = "/run/codespace/ca/bundle.pem"
)

type sourceMetadata struct {
	Version      int    `json:"version"`
	Fingerprint  string `json:"fingerprint"`
	Architecture string `json:"architecture"`
}

// Prepare validates the built-in Web IDE assets and prepares the persistent
// data directories and optional custom program for the current architecture.
func Prepare(ctx context.Context, archiveURL string, uid, gid uint32) error {
	for _, path := range []string{UserDataSourcePath, ExtensionsSourcePath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create Web IDE data directory: %w", err)
		}
		if err := os.Chown(path, int(uid), int(gid)); err != nil {
			return fmt.Errorf("set Web IDE data ownership: %w", err)
		}
	}
	if err := regularExecutable(filepath.Join(BuiltinProgramPath, "bin", "code-server")); err != nil && strings.TrimSpace(archiveURL) == "" {
		return fmt.Errorf("platform image does not contain the Web IDE program: %w", err)
	}
	if info, err := os.Stat(PlatformVSIXPath); err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("asset is not a regular file")
		}
		return fmt.Errorf("platform image does not contain the Codespace extension: %w", err)
	}
	archiveURL = strings.TrimSpace(archiveURL)
	if archiveURL == "" {
		return nil
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("custom Web IDE archives do not support runtime architecture %s", runtime.GOARCH)
	}
	resolvedURL := strings.ReplaceAll(archiveURL, "{arch}", runtime.GOARCH)
	fingerprintBytes := sha256.Sum256([]byte(resolvedURL))
	expected := sourceMetadata{Version: 1, Fingerprint: hex.EncodeToString(fingerprintBytes[:]), Architecture: runtime.GOARCH}
	if metadataMatches(CustomProgramPath, expected) && regularExecutable(filepath.Join(CustomProgramPath, "bin", "code-server")) == nil {
		return nil
	}
	return installCustomProgram(ctx, resolvedURL, expected)
}

func metadataMatches(programPath string, expected sourceMetadata) bool {
	data, err := os.ReadFile(filepath.Join(programPath, ".source.json"))
	if err != nil || len(data) > 4096 {
		return false
	}
	var actual sourceMetadata
	return json.Unmarshal(data, &actual) == nil && actual == expected
}

func regularExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable regular file", path)
	}
	return nil
}

func installCustomProgram(ctx context.Context, resolvedURL string, metadata sourceMetadata) error {
	if err := os.MkdirAll(PersistentRootPath, 0o700); err != nil {
		return fmt.Errorf("create Web IDE runtime directory: %w", err)
	}
	archive, err := os.CreateTemp(PersistentRootPath, ".web-ide-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create Web IDE archive: %w", err)
	}
	archivePath := archive.Name()
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archivePath)
	}()

	client, err := downloadClient()
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resolvedURL, nil)
	if err != nil {
		return fmt.Errorf("create Web IDE download request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download Web IDE archive from %s: %w", displayURL(resolvedURL), err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("download Web IDE archive from %s: HTTP %s", displayURL(resolvedURL), response.Status)
	}
	if response.ContentLength > maxArchiveBytes {
		return fmt.Errorf("web IDE archive exceeds %d bytes", maxArchiveBytes)
	}
	written, err := io.Copy(archive, io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return fmt.Errorf("download Web IDE archive: %w", err)
	}
	if written > maxArchiveBytes {
		return fmt.Errorf("web IDE archive exceeds %d bytes", maxArchiveBytes)
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close Web IDE archive: %w", err)
	}

	staged, err := os.MkdirTemp(PersistentRootPath, ".web-ide-program-*")
	if err != nil {
		return fmt.Errorf("create Web IDE staging directory: %w", err)
	}
	defer os.RemoveAll(staged)
	if err := extractStandalone(archivePath, staged); err != nil {
		return err
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return fmt.Errorf("set Web IDE program permissions: %w", err)
	}
	if err := regularExecutable(filepath.Join(staged, "bin", "code-server")); err != nil {
		return fmt.Errorf("web IDE archive does not contain bin/code-server: %w", err)
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staged, ".source.json"), data, 0o600); err != nil {
		return fmt.Errorf("write Web IDE source metadata: %w", err)
	}
	backup := CustomProgramPath + ".previous"
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove previous Web IDE program backup: %w", err)
	}
	hasBackup := false
	if err := os.Rename(CustomProgramPath, backup); err == nil {
		hasBackup = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stage previous Web IDE program: %w", err)
	}
	if err := os.Rename(staged, CustomProgramPath); err != nil {
		activateErr := fmt.Errorf("activate Web IDE program: %w", err)
		if !hasBackup {
			return activateErr
		}
		if restoreErr := os.Rename(backup, CustomProgramPath); restoreErr != nil {
			return errors.Join(activateErr, fmt.Errorf("restore previous Web IDE program: %w", restoreErr))
		}
		return activateErr
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove previous Web IDE program backup: %w", err)
	}
	return nil
}

func downloadClient() (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if data, readErr := os.ReadFile(platformCABundle); readErr == nil {
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("platform CA bundle contains no certificates")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read platform CA bundle: %w", readErr)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &http.Client{Transport: transport, Timeout: 15 * time.Minute}, nil
}

func displayURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return "configured URL"
	}
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	return parsed.String()
}

func extractStandalone(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open Web IDE archive: %w", err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open Web IDE gzip archive: %w", err)
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	root := ""
	var total int64
	for entries := 0; ; entries++ {
		if entries >= maxArchiveEntries {
			return fmt.Errorf("web IDE archive contains too many entries")
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read Web IDE archive: %w", err)
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		if name == "." || strings.HasPrefix(name, "../") || filepath.IsAbs(header.Name) {
			return fmt.Errorf("web IDE archive contains an unsafe path")
		}
		parts := strings.SplitN(name, "/", 2)
		if root == "" {
			root = parts[0]
		} else if parts[0] != root {
			return fmt.Errorf("web IDE archive must contain one top-level directory")
		}
		if len(parts) == 1 {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("web IDE archive top-level entry must be a directory")
			}
			continue
		}
		relative := filepath.FromSlash(parts[1])
		target := filepath.Join(destination, relative)
		if !isWithinDirectory(destination, target) {
			return fmt.Errorf("web IDE archive contains an escaping path")
		}
		if header.Size < 0 || header.Size > maxExtractedBytes-total {
			return fmt.Errorf("web IDE archive expands beyond %d bytes", maxExtractedBytes)
		}
		total += header.Size
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0o755)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(output, reader, header.Size)
			if err := errors.Join(copyErr, output.Close()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) {
				return fmt.Errorf("web IDE archive contains an absolute symbolic link")
			}
			linkTarget := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(header.Linkname)))
			if !isWithinDirectory(destination, linkTarget) {
				return fmt.Errorf("web IDE archive contains an escaping symbolic link")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
		default:
			return fmt.Errorf("web IDE archive contains unsupported entry type %d", header.Typeflag)
		}
	}
	if root == "" {
		return fmt.Errorf("web IDE archive is empty")
	}
	return nil
}

func isWithinDirectory(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
