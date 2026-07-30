// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package register

import (
	"bytes"
	"io"
	"testing"
)

func TestCommandPassesConfigAndStreams(t *testing.T) {
	input := bytes.NewBufferString("registration input")
	var output bytes.Buffer
	var gotConfig, gotInput string
	command := newCommand(func(writer io.Writer, reader io.Reader, configPath string) error {
		gotConfig = configPath
		value, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		gotInput = string(value)
		_, err = io.WriteString(writer, "registered")
		return err
	})
	command.SetArgs([]string{"--config", "/etc/gitea-codespace.yaml"})
	command.SetIn(input)
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotConfig != "/etc/gitea-codespace.yaml" || gotInput != "registration input" || output.String() != "registered" {
		t.Fatalf("command arguments = (%q, %q, %q)", gotConfig, gotInput, output.String())
	}
}
