// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package register

import (
	"io"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/app"
)

type registerFunc func(output io.Writer, input io.Reader, configPath string) error

// NewCommand creates the manager registration command.
func NewCommand() *cobra.Command {
	return newCommand(app.Register)
}

func newCommand(register registerFunc) *cobra.Command {
	var configPath string
	command := &cobra.Command{
		Use:   "register",
		Short: "Register this Codespace manager with Gitea",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return register(command.OutOrStdout(), command.InOrStdin(), configPath)
		},
	}
	command.Flags().StringVar(&configPath, "config", "", "Path to an existing YAML config")
	return command
}
