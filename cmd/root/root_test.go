// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package root

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/runtimecmd"
)

func TestHelpShowsOnlyPublicCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if status := Execute(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); status != 0 {
		t.Fatalf("Execute() status = %d, stderr = %q", status, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "serve") {
		t.Fatalf("help does not contain public commands:\n%s", output)
	}
	if strings.Contains(output, "runtime") {
		t.Fatalf("help contains hidden runtime command:\n%s", output)
	}
}

func TestUnknownCommandReportsOneError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if status := Execute(context.Background(), []string{"unknown"}, strings.NewReader(""), &stdout, &stderr); status != 1 {
		t.Fatalf("Execute() status = %d", status)
	}
	if count := strings.Count(stderr.String(), "unknown command"); count != 1 {
		t.Fatalf("unknown command error count = %d, stderr = %q", count, stderr.String())
	}
}

func TestRuntimeExitStatus(t *testing.T) {
	command := &cobra.Command{
		Use:           "gitea-codespace",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return &runtimecmd.ExitError{Status: 25}
		},
	}
	var stdout, stderr bytes.Buffer
	if status := execute(command, context.Background(), nil, strings.NewReader(""), &stdout, &stderr); status != 25 {
		t.Fatalf("execute() status = %d", status)
	}
	if stderr.Len() != 0 {
		t.Fatalf("execute() stderr = %q", stderr.String())
	}
}
