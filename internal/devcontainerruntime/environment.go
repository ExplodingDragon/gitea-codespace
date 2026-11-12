// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"context"
	"fmt"
	"os"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
)

// RuntimeEnvironmentPath is the Agent-generated authorization environment.
const RuntimeEnvironmentPath = "/run/codespace/environment/env.sh"

const containerEnvironmentPath = "/var/lib/gitea-codespace/environment/env.sh"

func prepareRuntimeEnvironment(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
	uid, gid, err := engine.RemoteUserIDs(ctx, state)
	if err != nil {
		return err
	}
	// remoteUser may retain its image UID when updateRemoteUserUID is false.
	if err := os.Chown(RuntimeEnvironmentPath, uid, gid); err != nil {
		return fmt.Errorf("set runtime authorization environment owner: %w", err)
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, "/", []string{"/bin/sh", "-c", `test -r "$1"`, "sh", containerEnvironmentPath}, nil, nil); err != nil {
		return fmt.Errorf("read runtime authorization environment as remote user: %w", err)
	}
	return nil
}
