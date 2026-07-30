// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

func newConnectCommand() *cobra.Command {
	var host string
	var port uint16
	command := &cobra.Command{
		Use:  "connect",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runtimecmd.Connect(command.Context(), host, port, command.InOrStdin(), command.OutOrStdout())
		},
	}
	command.Flags().StringVar(&host, "host", "localhost", "Dev Container loopback host")
	command.Flags().Uint16Var(&port, "port", 0, "Dev Container loopback port")
	return command
}
