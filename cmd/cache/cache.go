// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package cache defines the managed registry cache command.
package cache

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	cachepkg "gitea.dev/codespace/internal/cache"
	componentpkg "gitea.dev/codespace/internal/component"
	configpkg "gitea.dev/codespace/internal/config"
)

// NewCommand creates the managed registry cache component command.
func NewCommand() *cobra.Command {
	var id string
	command := &cobra.Command{
		Use:   "cache",
		Short: "Run a registry cache through the Manager API",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if !configpkg.ValidCacheID(id) {
				return fmt.Errorf("a valid cache ID is required")
			}
			connection, err := componentpkg.NewManagerConnection()
			if err != nil {
				return err
			}
			defer connection.Close()
			err = cachepkg.Run(ctx, command.OutOrStdout(), &componentpkg.CacheComponentClient{Client: connection.Service}, cachepkg.RunOptions{
				ID: id, CertificateDirectory: "/var/run/codespace/registry-tls",
			})
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
	command.Flags().StringVar(&id, "id", "", "Cache server ID configured in the administration panel")
	_ = command.MarkFlagRequired("id")
	return command
}
