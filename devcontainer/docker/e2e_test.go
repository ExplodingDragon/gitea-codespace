// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitea.dev/codespace/devcontainer"
)

// TestDockerE2EOfficialInterop follows the reference CLI's real-container test
// pattern: create from official assets, inspect behavior inside the primary
// container, then stop and resume the same environment.
func TestDockerE2EOfficialInterop(t *testing.T) {
	if os.Getenv("DEVCONTAINER_E2E") != "1" {
		t.Skip("Docker E2E is disabled; run make test-devcontainer-e2e-required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	workspace := t.TempDir()
	if err := os.CopyFS(workspace, os.DirFS("testdata/official-interop")); err != nil {
		t.Fatalf("copy official interoperability fixture: %v", err)
	}

	var output bytes.Buffer
	engine, err := New(ctx, &output, &output)
	if err != nil {
		t.Fatalf("create Docker engine: %v", err)
	}
	defer engine.Close()

	readyStage := devcontainer.LifecycleStage("")
	state, err := engine.Create(ctx, CreateOptions{
		OwnerID:         "devcontainer-e2e-" + uuid.NewString(),
		Workspace:       workspace,
		AllowedPathRoot: workspace,
		Source:          devcontainer.Source{Path: ".devcontainer/devcontainer.json"},
		HostUser: devcontainer.HostUser{
			Name: os.Getenv("USER"),
			UID:  uint32(os.Getuid()),
			GID:  uint32(os.Getgid()),
			Home: os.Getenv("HOME"),
		},
		Ready: func(_ *devcontainer.State, stage devcontainer.LifecycleStage) {
			readyStage = stage
		},
	})
	if err != nil {
		t.Fatalf("create official interoperability environment: %v\n%s", err, output.String())
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := engine.Delete(cleanupCtx, state); err != nil {
			t.Errorf("delete official interoperability environment: %v", err)
		}
	}()

	if readyStage != devcontainer.LifecycleStageUpdateContent {
		t.Fatalf("ready stage = %q", readyStage)
	}
	if state.ComposeProject == "" || state.PrimaryService != "workspace" || len(state.RelatedContainerIDs) != 1 {
		t.Fatalf("Compose state = %#v", state)
	}
	assertOfficialInteropEnvironment(t, ctx, engine, state, os.Getuid(), os.Getgid(), 1, 0)

	if err := engine.RunPostAttach(ctx, state, nil); err != nil {
		t.Fatalf("run first postAttachCommand: %v", err)
	}
	firstContainerID := state.PrimaryContainerID
	if _, err := engine.Stop(ctx, state); err != nil {
		t.Fatalf("stop official interoperability environment: %v", err)
	}
	state, err = engine.Start(ctx, state, nil)
	if err != nil {
		t.Fatalf("resume official interoperability environment: %v", err)
	}
	if state.PrimaryContainerID != firstContainerID {
		t.Fatalf("primary container changed from %s to %s", firstContainerID, state.PrimaryContainerID)
	}
	if err := engine.RunPostAttach(ctx, state, nil); err != nil {
		t.Fatalf("run resumed postAttachCommand: %v", err)
	}
	assertOfficialInteropEnvironment(t, ctx, engine, state, os.Getuid(), os.Getgid(), 2, 2)
}

func TestDockerE2EImageSource(t *testing.T) {
	if os.Getenv("DEVCONTAINER_E2E") != "1" {
		t.Skip("Docker E2E is disabled; run make test-devcontainer-e2e-required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	workspace := t.TempDir()
	if err := os.CopyFS(workspace, os.DirFS("testdata/image-source")); err != nil {
		t.Fatalf("copy image-source fixture: %v", err)
	}

	var output bytes.Buffer
	engine, err := New(ctx, &output, &output)
	if err != nil {
		t.Fatalf("create Docker engine: %v", err)
	}
	defer engine.Close()
	state, err := engine.Create(ctx, CreateOptions{
		OwnerID:         "devcontainer-image-e2e-" + uuid.NewString(),
		Workspace:       workspace,
		AllowedPathRoot: workspace,
		Source:          devcontainer.Source{Path: ".devcontainer/devcontainer.json"},
		HostUser: devcontainer.HostUser{
			Name: os.Getenv("USER"), UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), Home: os.Getenv("HOME"),
		},
	})
	if err != nil {
		t.Fatalf("create image-source environment: %v\n%s", err, output.String())
	}
	defer func() {
		if state == nil {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := engine.Delete(cleanupCtx, state); err != nil {
			t.Errorf("delete image-source environment: %v", err)
		}
	}()

	if state.ComposeProject != "" || state.Configuration.Image == "" {
		t.Fatalf("image-source state = %#v", state)
	}
	if _, inherited := state.RemoteEnvironment["REMOVE_FROM_REMOTE"]; inherited {
		t.Fatalf("remoteEnv null did not remove inherited variable: %#v", state.RemoteEnvironment)
	}
	if _, stderr, err := engine.Exec(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir,
		[]string{"/bin/sh", "-c", `test "$IMAGE_SOURCE" = ready && test "$(cat .image-source-created)" = created`}, state.RemoteEnvironment, nil); err != nil {
		t.Fatalf("verify image-source environment: %v\n%s", err, strings.TrimSpace(string(stderr)))
	}
	if err := engine.Delete(ctx, state); err != nil {
		t.Fatalf("delete image-source environment: %v", err)
	}
	state = nil
	if strings.Contains(output.String(), "No resource found to remove for project") {
		t.Fatalf("image-source cleanup invoked Docker Compose:\n%s", output.String())
	}
}

func assertOfficialInteropEnvironment(t *testing.T, ctx context.Context, engine *Engine, state *devcontainer.State, uid, gid, startCount, attachCount int) {
	t.Helper()
	command := fmt.Sprintf(`set -eu
test "$(id -u)" = %s
test "$(id -g)" = %s
test "$IMAGE_VALUE" = image-metadata
test "$REPOSITORY_VALUE" = repository
test "$FROM_IMAGE" = image-metadata
test "$REMOTE_VALUE" = repository
test "$IMAGE_REMOTE" = image-metadata
command -v zsh >/dev/null
test "$(cat /workspace/.image-on-create)" = image
test "$(cat /workspace/.on-create)" = repository
test "$(cat /workspace/.update-content)" = update
test "$(cat /workspace/.post-create-first)" = first
test "$(cat /workspace/.post-create-second)" = second
test "$(wc -c < /workspace/.post-start)" = %d
if [ %d -gt 0 ]; then test "$(wc -c < /workspace/.post-attach)" = %d; fi`,
		strconv.Itoa(uid), strconv.Itoa(gid), startCount, attachCount, attachCount)
	if _, stderr, err := engine.Exec(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/sh", "-c", command}, state.RemoteEnvironment, nil); err != nil {
		t.Fatalf("verify official interoperability environment: %v\n%s", err, strings.TrimSpace(string(stderr)))
	}
	if filepath.Clean(state.RemoteWorkdir) != "/workspace" {
		t.Fatalf("remote workdir = %q", state.RemoteWorkdir)
	}
}
