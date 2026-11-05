// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/certificates"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
)

const (
	runtimeCADirectory  = runtimeRunDirectory + "/ca"
	runtimeCABundlePath = runtimeCADirectory + "/bundle.pem"
)

type siteTrust struct {
	giteaURL string
	customCA bool
}

func prepareSiteTrust(options *agentv1.RuntimeOptions, accessURL, identityDirectory string) (siteTrust, error) {
	if options == nil {
		return siteTrust{}, fmt.Errorf("runtime site trust is missing")
	}
	canonicalURL, err := canonicalGiteaURL(options.GiteaUrl)
	if err != nil {
		return siteTrust{}, err
	}
	canonicalAccessURL, err := canonicalGiteaURL(accessURL)
	if err != nil {
		return siteTrust{}, fmt.Errorf("runtime access Gitea URL: %w", err)
	}
	if canonicalAccessURL != canonicalURL {
		return siteTrust{}, fmt.Errorf("runtime access Gitea URL does not match the verified site")
	}
	bundle := strings.TrimSpace(options.GiteaCaBundle)
	u, _ := url.Parse(canonicalURL)
	if bundle != "" && u.Scheme != "https" {
		return siteTrust{}, fmt.Errorf("runtime Gitea CA bundle requires an HTTPS site")
	}
	if identityDirectory == "" {
		identityDirectory = "/run/identity"
	}
	internalRoot, err := os.ReadFile(filepath.Join(identityDirectory, "ca.crt"))
	if err != nil {
		return siteTrust{}, fmt.Errorf("read internal CA root: %w", err)
	}
	customCA, err := writePlatformCABundle(runtimeCADirectory, string(internalRoot), bundle)
	if err != nil {
		return siteTrust{}, fmt.Errorf("runtime platform CA bundle: %w", err)
	}
	return siteTrust{giteaURL: canonicalURL, customCA: customCA}, nil
}

func writePlatformCABundle(directory, internalBundle, siteBundle string) (bool, error) {
	internal, err := certificates.ParseCABundle(internalBundle)
	if err != nil {
		return false, fmt.Errorf("internal CA: %w", err)
	}
	site := map[string]*x509.Certificate{}
	if strings.TrimSpace(siteBundle) != "" {
		site, err = certificates.ParseCABundle(siteBundle)
		if err != nil {
			return false, fmt.Errorf("gitea site CA: %w", err)
		}
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return false, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name() != "bundle.pem" {
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				return false, err
			}
		}
	}
	bundlePath := filepath.Join(directory, "bundle.pem")
	internalFingerprints := make([]string, 0, len(internal))
	for fingerprint := range internal {
		internalFingerprints = append(internalFingerprints, fingerprint)
	}
	sort.Strings(internalFingerprints)
	siteFingerprints := make([]string, 0, len(site))
	for fingerprint := range site {
		if _, exists := internal[fingerprint]; exists {
			continue
		}
		siteFingerprints = append(siteFingerprints, fingerprint)
	}
	sort.Strings(siteFingerprints)
	fingerprints := slices.Concat(internalFingerprints, siteFingerprints)
	canonicalBundle := make([]byte, 0, len(internalBundle)+len(siteBundle))
	for _, fingerprint := range fingerprints {
		certificate, prefix := internal[fingerprint], "internal"
		if certificate == nil {
			certificate, prefix = site[fingerprint], "site"
		}
		encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
		canonicalBundle = append(canonicalBundle, encoded...)
		path := filepath.Join(directory, "codespace-"+prefix+"-"+fingerprint+".crt")
		if err := replaceCAFile(path, encoded); err != nil {
			return false, err
		}
	}
	return strings.TrimSpace(siteBundle) != "", replaceCAFile(bundlePath, canonicalBundle)
}

func replaceCAFile(path string, content []byte) error {
	return replaceFileAtomically(path, content, 0o444, nil)
}

func replaceFileAtomically(path string, content []byte, mode os.FileMode, owner *devcontainer.HostUser) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".replace-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if owner != nil {
		if err := temporary.Chown(int(owner.UID), int(owner.GID)); err != nil {
			_ = temporary.Close()
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func canonicalGiteaURL(raw string) (string, error) {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid Gitea URL")
	}
	return value, nil
}

func (t siteTrust) gitEnvironment(environment []string, includeCredential bool) []string {
	count := 0
	if includeCredential {
		count++
	}
	if t.customCA {
		count++
	}
	if count == 0 {
		return environment
	}
	environment = append(environment, fmt.Sprintf("GIT_CONFIG_COUNT=%d", count))
	index := 0
	if includeCredential {
		environment = append(environment, fmt.Sprintf("GIT_CONFIG_KEY_%d=credential.%s.helper", index, t.giteaURL), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", index, gitCredentialCommand(runtimeGiteaTokenPath)))
		index++
	}
	if t.customCA {
		environment = append(environment, fmt.Sprintf("GIT_CONFIG_KEY_%d=http.%s/.sslCAInfo", index, t.giteaURL), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", index, runtimeCABundlePath))
	}
	return environment
}
