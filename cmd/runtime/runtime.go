// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import "github.com/spf13/cobra"

// NewCommand creates the internal Dev Container runtime command tree.
func NewCommand() *cobra.Command {
	command := &cobra.Command{
		Use:    "runtime",
		Short:  "Run an internal Dev Container operation",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	command.AddCommand(
		newApplyCommand(),
		newCheckCommand(),
		newExecCommand(),
		newTCPCommand(),
		newConnectCommand(),
		newEndpointCommand(),
	)
	return command
}
