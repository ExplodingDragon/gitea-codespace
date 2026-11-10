// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package runtime defines commands injected into a Dev Container.
package runtime

import (
	"io"

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
			input := command.InOrStdin()
			stdin, ok := input.(io.ReadCloser)
			if !ok {
				stdin = io.NopCloser(input)
			}
			return runtimecmd.Connect(command.Context(), host, port, stdin, command.OutOrStdout())
		},
	}
	command.Flags().StringVar(&host, "host", "localhost", "Dev Container loopback host")
	command.Flags().Uint16Var(&port, "port", 0, "Dev Container loopback port")
	return command
}
