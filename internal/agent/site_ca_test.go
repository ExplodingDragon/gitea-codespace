// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.dev/codespace/internal/identity"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/stretchr/testify/require"
)

func TestWritePlatformCABundleRotation(t *testing.T) {
	certificate := func() string {
		authority, err := identity.NewAuthority(time.Now())
		require.NoError(t, err)
		return string(authority.RootPEM())
	}
	internal, first, second, proxy := certificate(), certificate(), certificate(), certificate()
	directory := t.TempDir()

	active, err := writePlatformCABundle(directory, internal, first, "")
	require.NoError(t, err)
	require.True(t, active)
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	active, err = writePlatformCABundle(directory, internal, second, proxy)
	require.NoError(t, err)
	require.True(t, active)
	entries, err = os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 4)
	bundle, err := os.ReadFile(filepath.Join(directory, "bundle.pem"))
	require.NoError(t, err)
	require.Equal(t, internal+second+proxy, string(bundle))
	info, err := os.Stat(filepath.Join(directory, "bundle.pem"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o444), info.Mode().Perm())

	environment := siteTrust{giteaURL: "https://gitea.example.com", customCA: true}.gitEnvironment(nil, true)
	require.ElementsMatch(t, []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.https://gitea.example.com.helper",
		"GIT_CONFIG_VALUE_0=" + gitCredentialCommand(runtimeGiteaTokenPath),
		"GIT_CONFIG_KEY_1=http.https://gitea.example.com/.sslCAInfo",
		"GIT_CONFIG_VALUE_1=/run/codespace/ca/bundle.pem",
	}, environment)

	active, err = writePlatformCABundle(directory, internal, "", "")
	require.NoError(t, err)
	require.False(t, active)
	entries, err = os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	bundle, err = os.ReadFile(filepath.Join(directory, "bundle.pem"))
	require.NoError(t, err)
	require.Equal(t, internal, string(bundle))
}

func TestRuntimePreparesDockerCacheTrust(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "")
	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	root := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	identityDirectory, dockerCertificates, caDirectory := t.TempDir(), t.TempDir(), t.TempDir()
	systemBundle := filepath.Join(caDirectory, "system.pem")
	dockerBundle := filepath.Join(caDirectory, "docker.pem")
	require.NoError(t, os.WriteFile(filepath.Join(identityDirectory, "ca.crt"), root, 0o600))
	require.NoError(t, os.WriteFile(systemBundle, []byte("system certificates\n"), 0o600))
	runtime := &Runtime{
		IdentityDirectory:          identityDirectory,
		DockerCertificateDirectory: dockerCertificates,
		systemCABundlePath:         systemBundle,
		dockerCABundlePath:         dockerBundle,
	}
	cache := &agentv1.RuntimeCache{
		BuildRegistry: "https://cache.codespace-system.svc/cache/example",
		Mirrors:       map[string]string{"ghcr.io": "https://cache.codespace-system.svc/mirror/ghcr.io"},
	}
	prepared, err := runtime.prepareDockerCacheTrust(cache)
	require.NoError(t, err)
	require.Equal(t, root, prepared)
	installed, err := os.ReadFile(filepath.Join(dockerCertificates, "cache.codespace-system.svc", "ca.crt"))
	require.NoError(t, err)
	require.Equal(t, root, installed)
	installed, err = os.ReadFile(dockerBundle)
	require.NoError(t, err)
	require.Equal(t, append([]byte("system certificates\n"), root...), installed)
	require.Equal(t, dockerBundle, os.Getenv("SSL_CERT_FILE"))

	cache.BuildRegistry = "http://cache.codespace-system.svc/cache/example"
	_, err = runtime.prepareDockerCacheTrust(cache)
	require.ErrorContains(t, err, "HTTPS")
}
