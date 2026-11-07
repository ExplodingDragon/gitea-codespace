// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package gateway defines the managed HTTP and SSH gateway command.
package gateway

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	componentpkg "gitea.dev/codespace/internal/component"
	gatewaypkg "gitea.dev/codespace/internal/gateway"
	"gitea.dev/codespace/internal/healthcheck"
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
			health := &healthcheck.State{}
			err = healthcheck.Run(ctx, healthcheck.Address(), health, func(runCtx context.Context) error {
				return gatewaypkg.Run(runCtx, command.OutOrStdout(), connection.Service, connection.Certificates, health)
			})
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
}
