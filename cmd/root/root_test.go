// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package root

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHelpShowsOnlyPublicCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if status := Execute(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); status != 0 {
		t.Fatalf("Execute() status = %d, stderr = %q", status, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "serve") || !strings.Contains(output, "gateway") {
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
