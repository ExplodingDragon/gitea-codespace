// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

type setEndpointFunc func(port uint16, label, scheme string, public bool) error
type deleteEndpointFunc func(port uint16) error

func newEndpointCommand() *cobra.Command {
	return newEndpointCommandWithRun(runtimecmd.SetEndpoint, runtimecmd.DeleteEndpoint)
}

func newEndpointCommandWithRun(setEndpoint setEndpointFunc, deleteEndpoint deleteEndpointFunc) *cobra.Command {
	command := &cobra.Command{Use: "endpoint", Args: cobra.NoArgs}
	listCommand := &cobra.Command{
		Use:  "list",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			endpoints, err := runtimecmd.ListEndpoints()
			if err != nil {
				return err
			}
			writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(writer, "PORT\tPROTOCOL\tVISIBILITY\tLABEL"); err != nil {
				return err
			}
			for _, endpoint := range endpoints {
				visibility := "private"
				if endpoint.Public {
					visibility = "public"
				}
				if _, err := fmt.Fprintf(writer, "%d\t%s\t%s\t%s\n", endpoint.UpstreamPort, endpoint.UpstreamScheme, visibility, endpoint.Label); err != nil {
					return err
				}
			}
			return writer.Flush()
		},
	}
	var label, scheme string
	var public bool
	setCommand := &cobra.Command{
		Use:  "set <port>",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			port, err := strconv.ParseUint(args[0], 10, 16)
			if err != nil || port == 0 {
				return fmt.Errorf("endpoint port is invalid")
			}
			return setEndpoint(uint16(port), label, scheme, public)
		},
	}
	setCommand.Flags().StringVar(&label, "label", "", "Display label (defaults to Port <port>)")
	setCommand.Flags().StringVar(&scheme, "protocol", "http", "Upstream protocol: http or https")
	setCommand.Flags().BoolVar(&public, "public", false, "Allow unauthenticated access after Gitea permission checks")
	deleteCommand := &cobra.Command{
		Use:  "delete <port>",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			port, err := strconv.ParseUint(args[0], 10, 16)
			if err != nil || port == 0 {
				return fmt.Errorf("endpoint port is invalid")
			}
			return deleteEndpoint(uint16(port))
		},
	}
	command.AddCommand(listCommand, setCommand, deleteCommand)
	return command
}
