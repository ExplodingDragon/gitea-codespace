// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"gitea.dev/codespace/internal/app"
	"gitea.dev/codespace/internal/runtimecmd"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "register":
		err = runRegister(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "runtime":
		err = runtimecmd.Run(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
	default:
		printUsage()
		os.Exit(1)
	}
	if err != nil {
		var exit *runtimecmd.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Status)
		}
		fmt.Fprintf(os.Stderr, "gitea-codespace %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func runRegister(args []string) error {
	flags := flag.NewFlagSet("register", flag.ExitOnError)
	configPath := flags.String("config", "", "Path to an existing config to merge; writes codespace.yaml by default.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return app.Register(os.Stdout, os.Stdin, *configPath)
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := flags.String("config", "", "Path to the codespace config file (.yaml or .yml).")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return app.Run(os.Stdout, *configPath)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  gitea-codespace register [--config path]\n")
	fmt.Fprintf(os.Stderr, "  gitea-codespace serve [--config path]\n")
}
