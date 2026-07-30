// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

type execFunc func(context.Context, runtimecmd.ExecOptions, io.Reader, io.Writer, io.Writer) error

func newExecCommand() *cobra.Command {
	return newExecCommandWithRun(runtimecmd.Exec)
}

func newExecCommandWithRun(run execFunc) *cobra.Command {
	var options runtimecmd.ExecOptions
	command := &cobra.Command{
		Use:  "exec [command...]",
		Args: cobra.ArbitraryArgs,
		RunE: func(command *cobra.Command, args []string) error {
			options.Command = args
			return run(command.Context(), options, command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
		},
	}
	command.Flags().StringVar(&options.StatePath, "state", "", "Absolute Dev Container state path")
	command.Flags().StringVar(&options.SecretsPath, "secrets", "", "Absolute runtime secrets path")
	command.Flags().BoolVar(&options.Interactive, "interactive", false, "Attach an interactive terminal")
	command.Flags().SetInterspersed(false)
	return command
}
