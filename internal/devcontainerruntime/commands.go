// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"context"
	_ "embed"
	"fmt"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
)

//go:embed builtin/endpoint.sh
var endpointScript []byte

//go:embed builtin/usbip.sh
var usbIPScript []byte

//go:embed builtin/gt.sh
var giteaCLIWrapper []byte

//go:embed builtin/install-gitea-cli.sh
var installGiteaCLIScript string

const (
	// ContainerRuntimeBinary serves SFTP and the in-container platform commands.
	ContainerRuntimeBinary  = "/usr/local/libexec/gitea-codespace-runtime"
	containerEndpointBinary = "/usr/local/bin/gitea-codespace-endpoint"
	containerUSBIPBinary    = "/usr/local/bin/gitea-codespace-usbip"
	platformGiteaCLIPath    = "/usr/local/libexec/gitea-codespace/gt"
	containerGiteaCLIPath   = "/usr/local/bin/gt"
)

func configureRuntimeCommands(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, request Request) error {
	if err := engine.Run(ctx, state.PrimaryContainerID, "0", "/", []string{"mkdir", "-p", "/usr/local/libexec", "/usr/local/bin"}, nil, nil); err != nil {
		return fmt.Errorf("prepare platform directories in Dev Container: %w", err)
	}
	if err := engine.CopyContent(ctx, state.PrimaryContainerID, containerEndpointBinary, 0o755, endpointScript); err != nil {
		return err
	}
	if request.USBIP.Enabled {
		if err := engine.CopyContent(ctx, state.PrimaryContainerID, containerUSBIPBinary, 0o755, usbIPScript); err != nil {
			return err
		}
	} else if err := engine.Run(ctx, state.PrimaryContainerID, "0", "/", []string{"rm", "-f", containerUSBIPBinary}, nil, nil); err != nil {
		return err
	}
	if request.DevContainer.GiteaCLI {
		if err := engine.Run(ctx, state.PrimaryContainerID, "0", "/", []string{"/bin/sh", "-c", installGiteaCLIScript}, nil, nil); err != nil {
			return fmt.Errorf("install Gitea CLI in Dev Container: %w", err)
		}
		if err := engine.CopyContent(ctx, state.PrimaryContainerID, containerGiteaCLIPath, 0o755, giteaCLIWrapper); err != nil {
			return err
		}
	}
	return nil
}
