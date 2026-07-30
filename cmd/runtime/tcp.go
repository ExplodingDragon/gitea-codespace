// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

func newTCPCommand() *cobra.Command {
	var statePath string
	var port uint16
	command := &cobra.Command{
		Use:  "tcp",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runtimecmd.TCP(command.Context(), statePath, port, command.InOrStdin(), command.OutOrStdout())
		},
	}
	command.Flags().StringVar(&statePath, "state", "", "Absolute Dev Container state path")
	command.Flags().Uint16Var(&port, "port", 0, "Dev Container loopback port")
	return command
}
