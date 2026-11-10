// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/certificates"
	"gitea.dev/codespace/internal/controlplane"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
)

const defaultSystemCABundlePath = "/etc/ssl/certs/ca-certificates.crt"

type runtimeTrust struct {
	giteaURL     string
	caBundlePath string
	caBundle     []byte
}

func (r *Runtime) prepareRuntimeTrust(options *agentv1.RuntimeOptions) (runtimeTrust, error) {
	if options == nil {
		return runtimeTrust{}, fmt.Errorf("runtime trust configuration is missing")
	}
	giteaURL, err := controlplane.CanonicalGiteaURL(options.GiteaUrl)
	if err != nil {
		return runtimeTrust{}, err
	}
	if strings.TrimSpace(options.GiteaCaBundle) != "" && !strings.HasPrefix(giteaURL, "https://") {
		return runtimeTrust{}, fmt.Errorf("runtime Gitea CA bundle requires an HTTPS site")
	}

	identityDirectory := r.IdentityDirectory
	if identityDirectory == "" {
		identityDirectory = "/run/identity"
	}
	systemPath := r.systemCABundlePath
	if systemPath == "" {
		systemPath = defaultSystemCABundlePath
	}
	bundlePath := r.outboundCABundlePath
	if bundlePath == "" {
		bundlePath = certificates.OutboundBundlePath
	}
	proxyPath := r.proxyCABundlePath
	if proxyPath == "" {
		proxyPath = certificates.ProxyCABundlePath
	}

	systemRoots, err := os.ReadFile(systemPath)
	if err != nil {
		return runtimeTrust{}, fmt.Errorf("read system CA bundle: %w", err)
	}
	internalRoot, err := os.ReadFile(filepath.Join(identityDirectory, "ca.crt"))
	if err != nil {
		return runtimeTrust{}, fmt.Errorf("read internal CA root: %w", err)
	}
	sources := []caBundleSource{{name: "system CA", content: string(systemRoots), systemComments: true}, {name: "internal CA", content: string(internalRoot)}}
	if site := strings.TrimSpace(options.GiteaCaBundle); site != "" {
		sources = append(sources, caBundleSource{name: "Gitea site CA", content: site})
	}
	proxy, err := os.ReadFile(proxyPath)
	if err == nil {
		sources = append(sources, caBundleSource{name: "proxy CA", content: string(proxy)})
	} else if !os.IsNotExist(err) {
		return runtimeTrust{}, fmt.Errorf("read proxy CA bundle: %w", err)
	}
	bundle, err := writeCompleteCABundle(bundlePath, sources)
	if err != nil {
		return runtimeTrust{}, fmt.Errorf("prepare runtime CA bundle: %w", err)
	}
	return runtimeTrust{giteaURL: giteaURL, caBundlePath: bundlePath, caBundle: bundle}, nil
}

func (t runtimeTrust) verifyAccessURL(value string) error {
	canonical, err := controlplane.CanonicalGiteaURL(value)
	if err != nil {
		return fmt.Errorf("runtime access Gitea URL: %w", err)
	}
	if canonical != t.giteaURL {
		return fmt.Errorf("runtime access Gitea URL does not match the verified site")
	}
	return nil
}

type caBundleSource struct {
	name           string
	content        string
	systemComments bool
}

func writeCompleteCABundle(path string, sources []caBundleSource) ([]byte, error) {
	all := make(map[string][]byte)
	for _, source := range sources {
		parse := certificates.ParseCABundle
		if source.systemComments {
			parse = certificates.ParseSystemCABundle
		}
		parsed, err := parse(source.content)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source.name, err)
		}
		for fingerprint, certificate := range parsed {
			all[fingerprint] = certificate.Raw
		}
	}
	fingerprints := make([]string, 0, len(all))
	for fingerprint := range all {
		fingerprints = append(fingerprints, fingerprint)
	}
	sort.Strings(fingerprints)
	bundle := make([]byte, 0)
	for _, fingerprint := range fingerprints {
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: all[fingerprint]})...)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := replaceCAFile(path, bundle); err != nil {
		return nil, err
	}
	return bundle, nil
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

func (t runtimeTrust) gitEnvironment(environment []string, includeCredential bool) []string {
	includeCA := strings.HasPrefix(t.giteaURL, "https://")
	count := 0
	if includeCredential {
		count++
	}
	if includeCA {
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
	if includeCA {
		environment = append(environment, fmt.Sprintf("GIT_CONFIG_KEY_%d=http.%s/.sslCAInfo", index, t.giteaURL), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", index, t.caBundlePath))
	}
	return environment
}

func dockerRegistryHosts(cache *agentv1.RuntimeCache, giteaURL string) (map[string]struct{}, error) {
	registries := map[string]struct{}{}
	if parsed, err := url.Parse(giteaURL); err == nil && parsed.Scheme == "https" {
		registries[parsed.Host] = struct{}{}
	}
	if cache == nil {
		return registries, nil
	}
	values := []string{cache.BuildRegistry}
	for _, mirror := range cache.Mirrors {
		values = append(values, mirror)
	}
	for _, value := range values {
		parsed, err := url.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("parse runtime cache registry: %w", err)
		}
		if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("runtime cache registry must be an absolute HTTPS URL without credentials, query, or fragment")
		}
		registries[parsed.Host] = struct{}{}
	}
	return registries, nil
}
