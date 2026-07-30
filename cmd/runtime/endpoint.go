// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

type setEndpointFunc func(id, label, scheme string, port uint16, public bool) error
type deleteEndpointFunc func(id string) error

func newEndpointCommand() *cobra.Command {
	return newEndpointCommandWithRun(runtimecmd.SetEndpoint, runtimecmd.DeleteEndpoint)
}

func newEndpointCommandWithRun(setEndpoint setEndpointFunc, deleteEndpoint deleteEndpointFunc) *cobra.Command {
	command := &cobra.Command{Use: "endpoint", Args: cobra.NoArgs}
	var public bool
	setCommand := &cobra.Command{
		Use:  "set <id> <label> <http|https> <port>",
		Args: cobra.ExactArgs(4),
		RunE: func(_ *cobra.Command, args []string) error {
			port, err := strconv.ParseUint(args[3], 10, 16)
			if err != nil || port == 0 {
				return fmt.Errorf("endpoint port is invalid")
			}
			return setEndpoint(args[0], args[1], args[2], uint16(port), public)
		},
	}
	setCommand.Flags().BoolVar(&public, "public", false, "Allow unauthenticated access after Gitea permission checks")
	deleteCommand := &cobra.Command{
		Use:  "delete <id>",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return deleteEndpoint(args[0])
		},
	}
	command.AddCommand(setCommand, deleteCommand)
	return command
}
