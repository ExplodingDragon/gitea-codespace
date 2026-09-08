// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package serve

import (
	"bytes"
	"io"
	"testing"
)

func TestCommandPassesOutput(t *testing.T) {
	var output bytes.Buffer
	command := newCommand(func(writer io.Writer) error {
		_, err := io.WriteString(writer, "started")
		return err
	})
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if output.String() != "started" {
		t.Fatalf("command output = %q", output.String())
	}
}
