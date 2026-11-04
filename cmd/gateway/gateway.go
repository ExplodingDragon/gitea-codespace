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
			connection, err := componentpkg.NewManagerConnection()
			if err != nil {
				return err
			}
			defer connection.Close()
			err = gatewaypkg.Run(ctx, command.OutOrStdout(), connection.Service, connection.Certificates)
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
}
