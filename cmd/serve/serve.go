// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package serve

import (
	"io"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/app"
)

type serveFunc func(output io.Writer, configPath string) error

// NewCommand creates the manager service command.
func NewCommand() *cobra.Command {
	return newCommand(app.Run)
}

func newCommand(serve serveFunc) *cobra.Command {
	var configPath string
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the Codespace manager and gateway",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return serve(command.OutOrStdout(), configPath)
		},
	}
	command.Flags().StringVar(&configPath, "config", "", "Path to the Codespace config file (.yaml or .yml)")
	return command
}
