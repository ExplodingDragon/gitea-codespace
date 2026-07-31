// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"io"
	"reflect"
	"testing"

	"gitea.dev/codespace/internal/runtimecmd"
)

func TestExecCommandPreservesTargetArguments(t *testing.T) {
	var got runtimecmd.ExecOptions
	command := newExecCommandWithRun(func(_ context.Context, options runtimecmd.ExecOptions, _ io.Reader, _, _ io.Writer) error {
		got = options
		return nil
	})
	command.SetArgs([]string{"--state", "/runtime/state.json", "--interactive", "/bin/tool", "--target", "value"})
	if err := command.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	wantCommand := []string{"/bin/tool", "--target", "value"}
	if got.StatePath != "/runtime/state.json" || !got.Interactive || !reflect.DeepEqual(got.Command, wantCommand) {
		t.Fatalf("ExecOptions = %#v", got)
	}
}

func TestEndpointCommandsMapArguments(t *testing.T) {
	var gotLabel, gotScheme string
	var gotPort uint16
	var gotPublic bool
	command := newEndpointCommandWithRun(func(port uint16, label, scheme string, public bool) error {
		gotLabel, gotScheme, gotPort, gotPublic = label, scheme, port, public
		return nil
	}, func(uint16) error {
		t.Fatal("delete called while testing set")
		return nil
	})
	command.SetArgs([]string{"set", "8443", "--label", "Web preview", "--protocol", "https", "--public"})
	if err := command.Execute(); err != nil {
		t.Fatalf("set Execute() error = %v", err)
	}
	if gotLabel != "Web preview" || gotScheme != "https" || gotPort != 8443 || !gotPublic {
		t.Fatalf("set arguments = (%q, %q, %d, %t)", gotLabel, gotScheme, gotPort, gotPublic)
	}

	var deletedPort uint16
	command = newEndpointCommandWithRun(func(uint16, string, string, bool) error {
		t.Fatal("set called while testing delete")
		return nil
	}, func(port uint16) error {
		deletedPort = port
		return nil
	})
	command.SetArgs([]string{"delete", "8443"})
	if err := command.Execute(); err != nil {
		t.Fatalf("delete Execute() error = %v", err)
	}
	if deletedPort != 8443 {
		t.Fatalf("deleted endpoint port = %d", deletedPort)
	}
}
