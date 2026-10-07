// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

func newSFTPCommand() *cobra.Command {
	var workdir string
	command := &cobra.Command{
		Use:  "sftp",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runtimecmd.SFTP(workdir, command.InOrStdin(), command.OutOrStdout())
		},
	}
	command.Flags().StringVar(&workdir, "workdir", "/", "Initial SFTP working directory")
	return command
}
