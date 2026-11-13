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
	command := &cobra.Command{
		Use: "usbip", Short: "Manage USB devices through registered SSH sources", Args: cobra.NoArgs,
		Long:    "Manage USB devices through registered SSH sources. Bus IDs identify remote devices; VHCI ports identify local imports and are not TCP ports.",
		Example: "  gitea-codespace-usbip list --source workstation\n  gitea-codespace-usbip attach 1-3 --source workstation\n  gitea-codespace-usbip status --format json\n  gitea-codespace-usbip detach 0",
	}
	add := func(use, description string, operation agentv1.RuntimeUSBIPOperation, arguments cobra.PositionalArgs) {
		format := "text"
		source := ""
		subcommand := &cobra.Command{
			Use: use, Short: description, Args: arguments,
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
	add("list", "Query remote exported devices; source errors are reported alongside successful results", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST, cobra.NoArgs)
	add("sources", "List registered SSH sources, independently of remote USB/IP server health", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_SOURCES, cobra.NoArgs)
	add("attach <bus-id>", "Import a remote bus ID; --source is required when multiple sources are registered", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_ATTACH, cobra.ExactArgs(1))
	add("status", "Read local VHCI imports, including devices whose source has disconnected", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_STATUS, cobra.NoArgs)
	add("detach <port>", "Disconnect a local VHCI port (including 0); unmount disks first", agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_DETACH, cobra.ExactArgs(1))
	return command
}
