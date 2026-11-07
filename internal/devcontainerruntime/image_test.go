// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	containerdocker "gitea.dev/codespace/devcontainer/docker"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestImageCustomizationRunsLifecycleAfterGiteaCLI(t *testing.T) {
	directory := t.TempDir()
	asset := filepath.Join(t.TempDir(), "gt")
	require.NoError(t, os.WriteFile(asset, []byte("binary"), 0o500))
	script := "case \"$1\" in build) touch /etc/configured ;; esac\n"
	customize := imageCustomization(Configuration{
		GiteaCLI:        true,
		LifecycleScript: script,
	}, asset, nil)
	require.NotNil(t, customize)
	require.NoError(t, customize(containerdocker.ImageCustomization{
		ContextDir: directory, BaseImage: "devcontainer-user:environment", ContainerUser: "developer",
		Platform: ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH},
	}))
	dockerfile, err := os.ReadFile(filepath.Join(directory, "Dockerfile"))
	require.NoError(t, err)
	content := string(dockerfile)
	cli := strings.Index(content, "gt completion fish")
	lifecycle := strings.Index(content, "/bin/sh /usr/local/lib/gitea-codespace/lifecycle.sh build")
	require.GreaterOrEqual(t, cli, 0)
	require.Greater(t, lifecycle, cli)
	require.True(t, strings.HasSuffix(content, "USER developer\n"))
	stored, err := os.ReadFile(filepath.Join(directory, "lifecycle.sh"))
	require.NoError(t, err)
	require.Equal(t, script, string(stored))
}

func TestImageCustomizationRequiresPlatformGiteaCLI(t *testing.T) {
	customize := imageCustomization(Configuration{GiteaCLI: true}, filepath.Join(t.TempDir(), "missing"), nil)
	require.ErrorContains(t, customize(containerdocker.ImageCustomization{
		ContextDir: t.TempDir(), BaseImage: "devcontainer-user:environment", ContainerUser: "developer",
		Platform: ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH},
	}), "platform image does not contain the Gitea CLI asset")
	require.ErrorContains(t, customize(containerdocker.ImageCustomization{
		ContextDir: t.TempDir(), BaseImage: "devcontainer-user:environment", ContainerUser: "developer",
		Platform: ocispec.Platform{OS: runtime.GOOS, Architecture: "unsupported"},
	}), "target image")
}
