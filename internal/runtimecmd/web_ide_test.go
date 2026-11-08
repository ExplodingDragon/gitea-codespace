// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPlatformExtensionIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitea-codespace.vsix")
	file, err := os.Create(path)
	require.NoError(t, err)
	archive := zip.NewWriter(file)
	manifest, err := archive.Create("extension/package.json")
	require.NoError(t, err)
	_, err = manifest.Write([]byte(`{"publisher":"gitea","name":"codespace","version":"1.2.3"}`))
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, file.Close())
	identity, err := platformExtensionIdentity(path)
	require.NoError(t, err)
	require.Equal(t, "gitea.codespace@1.2.3", identity)
}

func TestPrepareWebIDEWritesInitialSettings(t *testing.T) {
	dataHome := t.TempDir()
	require.NoError(t, prepareWebIDE(strings.NewReader(`{"editor.tabSize":2}`), dataHome))
	settingsPath := filepath.Join(dataHome, "User", "settings.json")
	settings, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.JSONEq(t, `{"editor.tabSize":2}`, string(settings))
	require.NoError(t, prepareWebIDE(strings.NewReader(`{"editor.tabSize":8}`), dataHome))
	settings, err = os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.JSONEq(t, `{"editor.tabSize":2}`, string(settings))
	require.Error(t, prepareWebIDE(strings.NewReader(`[]`), dataHome))
}

func TestWebIDESupervisorRestartsOneChild(t *testing.T) {
	directory := t.TempDir()
	countPath := filepath.Join(directory, "count")
	readyPath := filepath.Join(directory, "ready")
	t.Setenv("COUNT_FILE", countPath)
	t.Setenv("READY_FILE", readyPath)
	entrypoint := filepath.Join(directory, "entrypoint.sh")
	require.NoError(t, os.WriteFile(entrypoint, []byte(`#!/bin/sh
count=0
if test -f "$COUNT_FILE"; then count=$(cat "$COUNT_FILE"); fi
count=$((count + 1))
printf '%s' "$count" >"$COUNT_FILE"
if test "$count" -lt 2; then exit 1; fi
touch "$READY_FILE"
trap 'exit 0' TERM INT
while :; do sleep 1; done
`), 0o700))
	supervisor := webIDESupervisor{
		entrypoint: entrypoint, workdir: directory, lockPath: filepath.Join(directory, "web-ide.lock"), output: io.Discard,
		restartDelay: 10 * time.Millisecond, maximumDelay: 20 * time.Millisecond, stableRuntime: time.Hour,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- supervisor.run(ctx) }()
	require.Eventually(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, supervisor.run(t.Context()))
	count, err := os.ReadFile(countPath)
	require.NoError(t, err)
	require.Equal(t, "2", string(count))
	cancel()
	require.NoError(t, <-done)
}
