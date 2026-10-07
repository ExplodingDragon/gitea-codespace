// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestJournalRecovery(t *testing.T) {
	directory := t.TempDir()
	identity := VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"}
	j, err := OpenJournal(directory, identity)
	require.NoError(t, err)
	_, err = OpenJournal(directory, identity)
	require.ErrorContains(t, err, "active writer")
	_, err = j.GitSSHKey("ed25519", false)
	require.ErrorContains(t, err, "identity recovery")
	key, err := j.GitSSHKey("ed25519", true)
	require.NoError(t, err)
	public := key.PublicKey().Marshal()
	_, err = j.GitSSHKey("rsa-4096", true)
	require.ErrorContains(t, err, "pinned algorithm")
	calls := 0
	run := func(context.Context) error { calls++; return nil }
	require.NoError(t, j.RunStage(t.Context(), 1, "onCreateCommand", run))
	require.NoError(t, j.Close())

	_, err = OpenJournal(directory, VolumeIdentity{SiteUID: "other", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.ErrorContains(t, err, "another Codespace")
	j, err = OpenJournal(directory, identity)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, j.Close()) })
	key, err = j.GitSSHKey("ed25519", false)
	require.NoError(t, err)
	require.Equal(t, public, key.PublicKey().Marshal())
	require.NoError(t, j.RunStage(t.Context(), 1, "onCreateCommand", run))
	require.Equal(t, 1, calls)

	j.record.Stages["postCreateCommand"] = stageResult{}
	require.NoError(t, j.save())
	require.ErrorContains(t, j.RunStage(t.Context(), 1, "postCreateCommand", run), "unknown result")
	require.Equal(t, 1, calls)
	require.ErrorContains(t, j.RunStage(t.Context(), 1, "build", func(context.Context) error {
		return errors.New("build failed")
	}), "build failed")
	require.ErrorContains(t, j.RunStage(t.Context(), 1, "build", run), "previously failed")
	require.Equal(t, 1, calls)
	require.NoError(t, j.RunStage(t.Context(), 2, "postStartCommand/pod-two", run))
	require.Equal(t, 2, calls)
	require.ErrorContains(t, j.RunStage(t.Context(), 1, "onCreateCommand", run), "stale")
	info, err := os.Stat(filepath.Join(directory, "identity/git-key"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestJournalSerialExecution(t *testing.T) {
	j, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	defer func() { require.NoError(t, j.Close()) }()
	started, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- j.RunStage(t.Context(), 1, "build", func(context.Context) error {
			close(started)
			<-finish
			return nil
		})
	}()
	<-started
	err = j.RunStage(t.Context(), 2, "stop", func(context.Context) error { return nil })
	close(finish)
	require.ErrorContains(t, err, "still running")
	require.NoError(t, <-done)
	require.NoError(t, j.RunStage(t.Context(), 2, "stop", func(context.Context) error { return nil }))
}

func TestJournalRejectsInvalidRecoveryFiles(t *testing.T) {
	directory := t.TempDir()
	identity := VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"}
	j, err := OpenJournal(directory, identity)
	require.NoError(t, err)
	j.record.Version = -1
	require.NoError(t, j.save())
	require.NoError(t, j.Close())
	_, err = OpenJournal(directory, identity)
	require.ErrorContains(t, err, "execution record")
	require.NoError(t, os.Remove(filepath.Join(directory, "state/execution.json")))
	require.NoError(t, unix.Mkfifo(filepath.Join(directory, "state/execution.json"), 0o600))
	_, err = OpenJournal(directory, identity)
	require.ErrorContains(t, err, "state file")
}

func TestJournalMissingRecordPreservesExistingIdentity(t *testing.T) {
	directory := t.TempDir()
	identity := VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"}
	j, err := OpenJournal(directory, identity)
	require.NoError(t, err)
	_, err = j.GitSSHKey("ed25519", true)
	require.NoError(t, err)
	require.NoError(t, j.Close())
	private, err := os.ReadFile(filepath.Join(directory, "identity/git-key"))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(directory, "state/execution.json")))
	_, err = OpenJournal(directory, identity)
	require.ErrorContains(t, err, "existing volume")
	retained, err := os.ReadFile(filepath.Join(directory, "identity/git-key"))
	require.NoError(t, err)
	require.Equal(t, private, retained)
}
