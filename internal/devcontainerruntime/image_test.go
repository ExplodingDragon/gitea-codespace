// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"os"
	"path/filepath"
	"testing"

	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"github.com/stretchr/testify/require"
)

func TestImageCustomizationRunsTemplateBuild(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	script := "case \"$1\" in build) touch /etc/configured ;; esac\n"
	customize := imageCustomization(script)
	require.NotNil(t, customize)
	require.NoError(t, customize(containerdocker.ImageCustomization{
		ContextDir: directory, BaseImage: "devcontainer-user:environment", ContainerUser: "developer",
	}))
	dockerfile, err := os.ReadFile(filepath.Join(directory, "Dockerfile"))
	require.NoError(t, err)
	require.Equal(t, "FROM devcontainer-user:environment\nUSER root\nCOPY lifecycle.sh "+templateLifecycleScriptPath+
		"\nRUN chmod 0500 "+templateLifecycleScriptPath+" && /bin/sh "+templateLifecycleScriptPath+" build\nUSER developer\n", string(dockerfile))
	stored, err := os.ReadFile(filepath.Join(directory, "lifecycle.sh"))
	require.NoError(t, err)
	require.Equal(t, script, string(stored))
	require.Nil(t, imageCustomization(" \n"))
}
