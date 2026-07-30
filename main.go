// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"

	rootcmd "gitea.dev/codespace/cmd/root"
)

func main() {
	os.Exit(rootcmd.Execute(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
