// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

// NewCommand creates the internal Dev Container runtime command tree.
func NewCommand() *cobra.Command {
	command := &cobra.Command{
		Use:    "runtime",
		Short:  "Run an internal Dev Container operation",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	command.AddCommand(
		newSFTPCommand(),
		newEndpointCommand(),
		newUSBIPCommand(),
		&cobra.Command{
			Use:   "event <type> <message>",
			Short: "Send an event to the Codespace log",
			Args:  cobra.ExactArgs(2),
			RunE: func(command *cobra.Command, args []string) error {
				return runtimecmd.EmitEvent(command.Context(), args[0], args[1])
			},
		},
	)
	return command
}
