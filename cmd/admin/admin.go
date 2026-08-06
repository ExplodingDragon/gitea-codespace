// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package admin

import (
	"io"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/app"
)

type runFunc func(output io.Writer) error

// NewCommand creates the local Manager administration command.
func NewCommand() *cobra.Command {
	return newCommand(app.RunAdmin)
}

func newCommand(run runFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "admin",
		Short: "Run the local Codespace manager administration API",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return run(command.OutOrStdout())
		},
	}
}
