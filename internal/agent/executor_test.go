// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/devcontainerruntime"
	"github.com/stretchr/testify/require"
)

func TestExecutorReusesCompletedCreate(t *testing.T) {
	j, err := OpenJournal(t.TempDir(), VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"})
	require.NoError(t, err)
	defer func() { require.NoError(t, j.Close()) }()
	state := &devcontainer.State{
		Version: devcontainer.StateFormatVersion, OwnerID: "runtime", ID: "environment",
		PrimaryContainerID: "container", ConfigurationPath: "/workspaces/repo/.devcontainer/devcontainer.json", ConfigurationSHA256: strings.Repeat("a", 64),
		Workspace: "/workspaces/repo", WorkspaceFolder: "/workspaces/repo", RemoteUser: "developer", RemoteWorkdir: "/workspaces/repo",
	}
	require.NoError(t, state.Validate())
	require.NoError(t, j.RunStage(t.Context(), 1, "create", func(context.Context) error {
		data, err := json.Marshal(state)
		require.NoError(t, err)
		return j.write("state/environment.json", data)
	}))
	executor := &Executor{Journal: j, PodUID: "new-pod", Stdout: io.Discard, Stderr: io.Discard}
	request := devcontainerruntime.Request{Version: devcontainerruntime.FormatVersion, Action: "create", CodespaceUUID: "runtime", OperationVersion: 1, Workspace: "/workspaces/repo", CodeServerVersion: "4.121.0"}
	recovered, err := executor.Apply(t.Context(), request)
	require.NoError(t, err)
	want, err := json.Marshal(state)
	require.NoError(t, err)
	got, err := json.Marshal(recovered)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))
	request.CodespaceUUID = "another-runtime"
	_, err = executor.Apply(t.Context(), request)
	require.ErrorContains(t, err, "identity")

	request.CodespaceUUID, request.OperationVersion = "runtime", 2
	j.record.Version = 2
	j.record.Stages = map[string]stageResult{"create": {}}
	require.NoError(t, j.save())
	_, err = executor.Apply(t.Context(), request)
	require.ErrorContains(t, err, "unknown result")
}
