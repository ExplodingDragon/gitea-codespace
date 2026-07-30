// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package serve

import (
	"bytes"
	"io"
	"testing"
)

func TestCommandPassesConfigAndOutput(t *testing.T) {
	var output bytes.Buffer
	var gotConfig string
	command := newCommand(func(writer io.Writer, configPath string) error {
		gotConfig = configPath
		_, err := io.WriteString(writer, "started")
		return err
	})
	command.SetArgs([]string{"--config", "/etc/gitea-codespace.yaml"})
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotConfig != "/etc/gitea-codespace.yaml" || output.String() != "started" {
		t.Fatalf("command arguments = (%q, %q)", gotConfig, output.String())
	}
}
