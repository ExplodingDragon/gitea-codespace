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

	environment := runtimeLocalEnvironment(devcontainer.HostUser{Name: "developer", Home: "/home/developer"})
	require.Equal(t, map[string]string{
		"HOME": "/home/developer", "LOGNAME": "developer", "USER": "developer",
		"PATH": "/usr/local/bin:/usr/bin", "SHELL": "/bin/bash",
	}, environment)
}
