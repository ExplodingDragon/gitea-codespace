// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

func newApplyCommand() *cobra.Command {
	var requestPath, resultPath string
	command := &cobra.Command{
		Use:  "apply",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runtimecmd.Apply(command.Context(), requestPath, resultPath, command.OutOrStdout(), command.ErrOrStderr())
		},
	}
	command.Flags().StringVar(&requestPath, "request", "", "Absolute runtime request path")
	command.Flags().StringVar(&resultPath, "result", "", "Absolute runtime result path")
	return command
}
