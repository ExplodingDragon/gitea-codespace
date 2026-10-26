// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package root

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/cmd/agent"
	"gitea.dev/codespace/cmd/cache"
	"gitea.dev/codespace/cmd/gateway"
	runtimecommand "gitea.dev/codespace/cmd/runtime"
	"gitea.dev/codespace/cmd/serve"
)

// NewCommand builds a fresh gitea-codespace command tree.
func NewCommand() *cobra.Command {
	command := &cobra.Command{
		Use:           "gitea-codespace",
		Short:         "Run a Gitea Codespace manager",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.CompletionOptions.DisableDefaultCmd = true
	command.AddCommand(serve.NewCommand(), gateway.NewCommand(), cache.NewCommand(), agent.NewCommand(), runtimecommand.NewCommand())
	return command
}

// Execute runs the command tree with explicit process resources and returns an exit status.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	command := NewCommand()
	command.SetArgs(args)
	command.SetIn(stdin)
	command.SetOut(stdout)
	command.SetErr(stderr)
	if err := command.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "gitea-codespace: %v\n", err)
		return 1
	}
	return 0
}
