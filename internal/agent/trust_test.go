// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.dev/codespace/internal/certificates"
	configpkg "gitea.dev/codespace/internal/config"
	"gitea.dev/codespace/internal/identity"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/stretchr/testify/require"
)

func testRootCA(t *testing.T) string {
	t.Helper()
	authority, err := identity.NewAuthority(time.Now())
	require.NoError(t, err)
	return string(authority.RootPEM())
}

func TestRuntimePreparesCompleteTrustBundle(t *testing.T) {
	systemRoot, internalRoot, siteRoot, proxyRoot := testRootCA(t), testRootCA(t), testRootCA(t), testRootCA(t)
	identityDirectory, runDirectory := t.TempDir(), t.TempDir()
	systemPath := filepath.Join(runDirectory, "system.pem")
	proxyPath := filepath.Join(runDirectory, "proxy.pem")
	bundlePath := filepath.Join(runDirectory, "ca", "bundle.pem")
	require.NoError(t, os.WriteFile(filepath.Join(identityDirectory, "ca.crt"), []byte(internalRoot), 0o600))
	require.NoError(t, os.WriteFile(systemPath, []byte("# system root\n"+systemRoot+"# additional root\n"+siteRoot), 0o600))
	require.NoError(t, os.WriteFile(proxyPath, []byte(proxyRoot+internalRoot), 0o600))

	runtime := &Runtime{IdentityDirectory: identityDirectory, systemCABundlePath: systemPath, proxyCABundlePath: proxyPath, outboundCABundlePath: bundlePath}
	trust, err := runtime.prepareRuntimeTrust(&agentv1.RuntimeOptions{GiteaUrl: "https://gitea.example.com/", GiteaCaBundle: siteRoot})
	require.NoError(t, err)
	require.Equal(t, "https://gitea.example.com", trust.giteaURL)
	parsed, err := certificates.ParseCABundle(string(trust.caBundle))
	require.NoError(t, err)
	require.Len(t, parsed, 4)
	written, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	require.Equal(t, trust.caBundle, written)
	require.Equal(t, bundlePath, trust.caBundlePath)
	info, err := os.Stat(bundlePath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o444), info.Mode().Perm())

	repeated, err := runtime.prepareRuntimeTrust(&agentv1.RuntimeOptions{GiteaUrl: "https://gitea.example.com", GiteaCaBundle: siteRoot})
	require.NoError(t, err)
	require.Equal(t, trust.caBundle, repeated.caBundle)
	require.NoError(t, trust.verifyAccessURL("https://gitea.example.com/"))
	require.ErrorContains(t, trust.verifyAccessURL("https://other.example.com"), "does not match")
}

func TestRuntimeTrustConfiguresHTTPSGitAndDockerRegistries(t *testing.T) {
	trust := runtimeTrust{giteaURL: "https://gitea.example.com", caBundlePath: "/runtime/trust.pem"}
	environment := trust.gitEnvironment(nil, true)
	require.ElementsMatch(t, []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.https://gitea.example.com.helper",
		"GIT_CONFIG_VALUE_0=" + gitCredentialCommand(runtimeGiteaTokenPath),
		"GIT_CONFIG_KEY_1=http.https://gitea.example.com/.sslCAInfo",
		"GIT_CONFIG_VALUE_1=/runtime/trust.pem",
	}, environment)

	cache := &agentv1.RuntimeCache{
		BuildRegistry: "https://cache.codespace-system.svc/cache/example",
		Mirrors:       map[string]string{"ghcr.io": "https://cache.codespace-system.svc/mirror/" + configpkg.CacheUpstreamID("ghcr.io")},
	}
	registries, err := dockerRegistryHosts(cache, trust.giteaURL)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"gitea.example.com": {}, "cache.codespace-system.svc": {}}, registries)

	cache.BuildRegistry = "http://cache.codespace-system.svc/cache/example"
	_, err = dockerRegistryHosts(cache, trust.giteaURL)
	require.ErrorContains(t, err, "HTTPS")
}
