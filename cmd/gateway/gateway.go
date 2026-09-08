// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/app"
)

// NewCommand creates the network-only gateway command.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "gateway",
		Short: "Run the HTTP and SSH gateway using external etcd",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return app.RunGateway(ctx, command.OutOrStdout())
		},
	}
}
