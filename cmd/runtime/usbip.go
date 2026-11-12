// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/runtimecmd"
	"github.com/spf13/cobra"
)

func newUSBIPCommand() *cobra.Command {
	command := &cobra.Command{Use: "usbip", Args: cobra.NoArgs}
	add := func(use string, operation agentv1.RuntimeUSBIPOperation, arguments cobra.PositionalArgs) {
		format := "text"
		source := ""
		subcommand := &cobra.Command{
			Use: use, Args: arguments,
			PreRunE: func(_ *cobra.Command, _ []string) error {
				if format != "text" && format != "json" {
					return fmt.Errorf("USB/IP format must be text or json")
				}
				return nil
			},
			RunE: func(command *cobra.Command, args []string) error {
				argument := ""
				if len(args) > 0 {
					argument = args[0]
				}
				response, err := runtimecmd.RunUSBIP(command.Context(), operation, argument, source)
				if err != nil {
					return err
				}
				if format == "json" {
					document, err := runtimecmd.USBIPDocument(operation, response)
					if err != nil {
						return err
					}
					return json.NewEncoder(command.OutOrStdout()).Encode(document)
				}
				if strings.TrimSpace(response.Output) != "" {
					_, err = fmt.Fprintln(command.OutOrStdout(), response.Output)
				}
				if operation == agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST || operation == agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_SOURCES {
					for _, source := range response.Sources {
						if _, err := fmt.Fprintln(command.OutOrStdout(), source.Name); err != nil {
							return err
						}
						if source.Output != "" {
							if _, err := fmt.Fprintln(command.OutOrStdout(), source.Output); err != nil {
								return err
							}
						}
						if source.Error != "" {
							if _, err := fmt.Fprintf(command.ErrOrStderr(), "%s: %s\n", source.Name, source.Error); err != nil {
								return err
							}
						}
					}
				}
				return err
			},
		}
		if operation == agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST || operation == agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_STATUS || operation == agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_SOURCES {
			subcommand.Flags().StringVar(&format, "format", "text", "Output format: text or json")
		}
		if operation == agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST || operation == agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_ATTACH {
			subcommand.Flags().StringVar(&source, "source", "", "Registered SSH USB/IP source")
		}
		command.AddCommand(subcommand)
	}
	add("list", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST, cobra.NoArgs)
	add("sources", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_SOURCES, cobra.NoArgs)
	add("attach <bus-id>", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_ATTACH, cobra.ExactArgs(1))
	add("status", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_STATUS, cobra.NoArgs)
	add("detach <port>", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_DETACH, cobra.ExactArgs(1))
	return command
}
