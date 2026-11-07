// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"github.com/stretchr/testify/require"
)

func TestRuntimeAccessEnvironmentUsesPlatformIdentity(t *testing.T) {
	t.Parallel()

	environment, err := runtimeAccessEnvironment([]*codespacev1.RuntimeSecretEnvironmentVariable{
		{Name: "PROJECT_KEY", Value: "project"},
		{Name: "GITEA_INSTANCE_URL", Value: "https://untrusted.example"},
		{Name: "GITEA_TOKEN", Value: "untrusted"},
	}, "https://gitea.example.com", "scoped-token")
	require.NoError(t, err)
	require.Equal(t, "project", environment["PROJECT_KEY"])
	require.Equal(t, "https://gitea.example.com", environment["GITEA_INSTANCE_URL"])
	require.Equal(t, "scoped-token", environment["GITEA_TOKEN"])
}
