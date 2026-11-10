// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package cache defines the managed registry cache command.
package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	cachepkg "gitea.dev/codespace/internal/cache"
	"gitea.dev/codespace/internal/certificates"
	componentpkg "gitea.dev/codespace/internal/component"
	configpkg "gitea.dev/codespace/internal/config"
	"gitea.dev/codespace/internal/healthcheck"
	transportpkg "gitea.dev/codespace/internal/transport"
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
			if _, err := os.Stat(certificates.ProxyCABundlePath); err == nil {
				if err := transportpkg.ExtendDefaultRootCAs(certificates.ProxyCABundlePath); err != nil {
					return fmt.Errorf("configure proxy CA: %w", err)
				}
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect proxy CA: %w", err)
			}
			connection, err := componentpkg.NewManagerConnection()
			if err != nil {
				return err
			}
			defer connection.Close()
			health := &healthcheck.State{}
			err = healthcheck.Run(ctx, healthcheck.Address(), health, func(runCtx context.Context) error {
				return cachepkg.Run(runCtx, command.OutOrStdout(), &componentpkg.CacheComponentClient{Client: connection.Service}, cachepkg.RunOptions{
					ID: id, CertificateDirectory: "/var/run/codespace/registry-tls",
				}, health)
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
