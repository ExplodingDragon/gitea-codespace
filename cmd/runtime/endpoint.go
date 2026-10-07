// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/runtimecmd"
)

type listEndpointsFunc func(context.Context) ([]*codespacev1.RuntimeEndpoint, error)
type setEndpointFunc func(context.Context, uint16, string, bool) error
type deleteEndpointFunc func(context.Context, uint16) error

func newEndpointCommand() *cobra.Command {
	return newEndpointCommandWithRun(runtimecmd.ListEndpoints, runtimecmd.SetEndpoint, runtimecmd.DeleteEndpoint)
}

func newEndpointCommandWithRun(listEndpoints listEndpointsFunc, setEndpoint setEndpointFunc, deleteEndpoint deleteEndpointFunc) *cobra.Command {
	command := &cobra.Command{Use: "endpoint", Args: cobra.NoArgs}
	listCommand := &cobra.Command{
		Use:  "list",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			endpoints, err := listEndpoints(command.Context())
			if err != nil {
				return err
			}
			writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(writer, "PORT\tVISIBILITY\tLABEL"); err != nil {
				return err
			}
			for _, endpoint := range endpoints {
				visibility := "private"
				if endpoint.Public {
					visibility = "public"
				}
				if _, err := fmt.Fprintf(writer, "%d\t%s\t%s\n", endpoint.Port, visibility, endpoint.Label); err != nil {
					return err
				}
			}
			return writer.Flush()
		},
	}
	var label string
	var public bool
	setCommand := &cobra.Command{
		Use:  "set <port>",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			port, err := strconv.ParseUint(args[0], 10, 16)
			if err != nil || port == 0 {
				return fmt.Errorf("endpoint port is invalid")
			}
			return setEndpoint(command.Context(), uint16(port), label, public)
		},
	}
	setCommand.Flags().StringVar(&label, "label", "", "Display label (defaults to Port <port>)")
	setCommand.Flags().BoolVar(&public, "public", false, "Allow unauthenticated access after Gitea permission checks")
	deleteCommand := &cobra.Command{
		Use:  "delete <port>",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			port, err := strconv.ParseUint(args[0], 10, 16)
			if err != nil || port == 0 {
				return fmt.Errorf("endpoint port is invalid")
			}
			return deleteEndpoint(command.Context(), uint16(port))
		},
	}
	command.AddCommand(listCommand, setCommand, deleteCommand)
	return command
}
