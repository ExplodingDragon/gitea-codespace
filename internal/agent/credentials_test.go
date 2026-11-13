// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/devcontainerruntime"
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
	_, err = runtimeAccessEnvironment(nil, "https://gitea.example.com", "token\x00value")
	require.ErrorContains(t, err, "NUL")
}

func TestAccessEnvironmentFileRoundTripAndReplacement(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "environment", "env.sh")
	owner := devcontainer.HostUser{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	environment, err := runtimeAccessEnvironment([]*codespacev1.RuntimeSecretEnvironmentVariable{
		{Name: "PROJECT_KEY", Value: "  quote'\"\\\n\r\t${HOME} $(touch injected) `touch injected`  "},
		{Name: "EMPTY_VALUE", Value: ""},
		{Name: "MULTILINE", Value: "first\\\nsecond\n"},
		{Name: "UNICODE", Value: "\u4e2d\u6587"},
	}, "https://gitea.example.com", "first-token")
	require.NoError(t, err)
	previous := map[string]os.FileInfo{}
	for _, token := range []string{"first-token", "replacement-token"} {
		environment["GITEA_TOKEN"] = token
		if token == "first-token" {
			environment["REMOVED_SECRET"] = "old-value"
		} else {
			delete(environment, "REMOVED_SECRET")
		}
		require.NoError(t, writeAccessEnvironment(path, environment, owner))
		for _, name := range []string{"env.sh", "systemd.env", "systemd.conf"} {
			info, err := os.Stat(filepath.Join(filepath.Dir(path), name))
			require.NoError(t, err)
			mode := os.FileMode(0o600)
			if name == "systemd.conf" {
				mode = 0o644
			}
			require.Equal(t, mode, info.Mode().Perm(), name)
			if previous[name] != nil {
				require.False(t, os.SameFile(previous[name], info), name)
			}
			previous[name] = info
		}
		systemd, err := os.ReadFile(filepath.Join(filepath.Dir(path), "systemd.env"))
		require.NoError(t, err)
		expected := "EMPTY_VALUE=\"\"\n" +
			"GITEA_INSTANCE_URL=\"https://gitea.example.com\"\n" +
			"GITEA_TOKEN=\"" + token + "\"\n" +
			"MULTILINE=\"first\\\\\nsecond\n\"\n" +
			"PROJECT_KEY=\"  quote'\\\"\\\\\n\r\t\\${HOME} \\$(touch injected) \\`touch injected\\`  \"\n"
		if token == "first-token" {
			expected += "REMOVED_SECRET=\"old-value\"\n"
		}
		expected += "UNICODE=\"\u4e2d\u6587\"\n"
		require.Equal(t, expected, string(systemd))
		dropIn, err := os.ReadFile(filepath.Join(filepath.Dir(path), "systemd.conf"))
		require.NoError(t, err)
		require.Equal(t, "[Service]\nEnvironmentFile="+devcontainerruntime.ContainerSystemdEnvironmentPath+"\n", string(dropIn))
		command := exec.CommandContext(t.Context(), "/bin/sh", "-c", `. "$1"; exec env -0`, "sh", path)
		command.Dir = directory
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "GITEA_TOKEN=stale"}
		output, err := command.Output()
		require.NoError(t, err)
		values := map[string]string{}
		for _, item := range bytes.Split(output, []byte{0}) {
			if name, value, ok := strings.Cut(string(item), "="); ok {
				values[name] = value
			}
		}
		for name, value := range environment {
			require.Equal(t, value, values[name], name)
		}
		if token == "replacement-token" {
			require.NotContains(t, values, "REMOVED_SECRET")
		}
		require.NoFileExists(t, filepath.Join(directory, "injected"))
	}
}

func TestAccessEnvironmentRejectsUnsupportedValuesBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"\x00", "\xff", "\uFEFF", "\uFDD0", "\uFFFF", "\U0010FFFF"} {
		directory := t.TempDir()
		path := filepath.Join(directory, "environment", "env.sh")
		err := writeAccessEnvironment(path, map[string]string{"PROJECT_KEY": "sensitive" + value}, devcontainer.HostUser{})
		require.ErrorContains(t, err, "PROJECT_KEY")
		require.NotContains(t, err.Error(), "sensitive")
		require.NoDirExists(t, filepath.Dir(path))
	}
}
