// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

func newCheckCommand() *cobra.Command {
	var statePath string
	command := &cobra.Command{
		Use:  "check",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runtimecmd.Check(command.Context(), statePath, command.OutOrStdout(), command.ErrOrStderr())
		},
	}
	command.Flags().StringVar(&statePath, "state", "", "Absolute Dev Container state path")
	return command
}
