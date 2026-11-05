// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	"gitea.dev/codespace/devcontainer"
	"github.com/stretchr/testify/require"
)

func TestRuntimeLocalEnvironmentExcludesProcessSecrets(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin")
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("CODESPACE_PRIVATE_TOKEN", "secret")
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", ".svc")

	environment := runtimeLocalEnvironment(devcontainer.HostUser{Name: "developer", Home: "/home/developer"})
	require.Equal(t, map[string]string{
		"HOME": "/home/developer", "LOGNAME": "developer", "USER": "developer",
		"PATH": "/usr/local/bin:/usr/bin", "SHELL": "/bin/bash",
		"HTTPS_PROXY": "http://proxy.example:3128", "NO_PROXY": ".svc",
	}, environment)
}
