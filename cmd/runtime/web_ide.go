// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

func newWebIDECommand() *cobra.Command {
	command := &cobra.Command{Use: "web-ide", Args: cobra.NoArgs}
	command.AddCommand(
		&cobra.Command{
			Use:  "serve",
			Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				return runtimecmd.SuperviseWebIDE(ctx)
			},
		},
		&cobra.Command{
			Use:  "wait",
			Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				return runtimecmd.WaitForWebIDE(command.Context())
			},
		},
	)
	return command
}
