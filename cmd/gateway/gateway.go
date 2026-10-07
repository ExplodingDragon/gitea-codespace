// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	componentpkg "gitea.dev/codespace/internal/component"
	gatewaypkg "gitea.dev/codespace/internal/gateway"
)

// NewCommand creates the network-only gateway command.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "gateway",
		Short: "Run the HTTP and SSH gateway using the Manager API",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			client, httpClient, err := componentpkg.NewComponentRPCClient()
			if err != nil {
				return err
			}
			defer httpClient.CloseIdleConnections()
			err = gatewaypkg.Run(ctx, command.OutOrStdout(), client, "")
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
}
