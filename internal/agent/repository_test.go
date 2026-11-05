// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGitCredentialCommand(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("secret-token"), 0o600))
	command := exec.Command("git", "-c", "credential.helper=", "-c", "credential.helper="+gitCredentialCommand(tokenPath), "credential", "fill")
	command.Stdin = strings.NewReader("protocol=https\nhost=gitea.example.com\n\n")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Contains(t, string(output), "username=codespace\n")
	require.Contains(t, string(output), "password=secret-token\n")
}

func TestGitCredentialScope(t *testing.T) {
	t.Parallel()

	trust := siteTrust{giteaURL: "https://gitea.example.com/root"}
	environment := trust.gitEnvironment([]string{"HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1"}, true)
	lookup := func(rawURL string) ([]byte, error) {
		command := exec.Command("git", "config", "--get-urlmatch", "credential.helper", rawURL)
		command.Env = environment
		return command.Output()
	}
	output, err := lookup("https://gitea.example.com/root/owner/repository.git")
	require.NoError(t, err)
	require.Equal(t, gitCredentialCommand(runtimeGiteaTokenPath), strings.TrimSpace(string(output)))
	_, err = lookup("https://example.com/owner/repository.git")
	require.Error(t, err)
}

func TestRepositoryCloneEndpoint(t *testing.T) {
	tests := []struct {
		url      string
		protocol string
		endpoint string
	}{
		{url: "https://gitea.example.com:8443/owner/repository.git", protocol: "HTTPS", endpoint: "gitea.example.com:8443"},
		{url: "ssh://git@gitea.example.com:2222/owner/repository.git", protocol: "SSH", endpoint: "gitea.example.com:2222"},
		{url: "git@gitea.example.com:owner/repository.git", protocol: "SSH", endpoint: "gitea.example.com"},
	}
	for _, test := range tests {
		protocol, endpoint := repositoryCloneEndpoint(test.url)
		require.Equal(t, test.protocol, protocol)
		require.Equal(t, test.endpoint, endpoint)
	}
}
