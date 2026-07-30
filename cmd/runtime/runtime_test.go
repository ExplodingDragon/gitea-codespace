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
	var gotID, gotLabel, gotScheme string
	var gotPort uint16
	var gotPublic bool
	command := newEndpointCommandWithRun(func(id, label, scheme string, port uint16, public bool) error {
		gotID, gotLabel, gotScheme, gotPort, gotPublic = id, label, scheme, port, public
		return nil
	}, func(string) error {
		t.Fatal("delete called while testing set")
		return nil
	})
	command.SetArgs([]string{"set", "preview", "Web preview", "https", "8443", "--public"})
	if err := command.Execute(); err != nil {
		t.Fatalf("set Execute() error = %v", err)
	}
	if gotID != "preview" || gotLabel != "Web preview" || gotScheme != "https" || gotPort != 8443 || !gotPublic {
		t.Fatalf("set arguments = (%q, %q, %q, %d, %t)", gotID, gotLabel, gotScheme, gotPort, gotPublic)
	}

	var deletedID string
	command = newEndpointCommandWithRun(func(string, string, string, uint16, bool) error {
		t.Fatal("set called while testing delete")
		return nil
	}, func(id string) error {
		deletedID = id
		return nil
	})
	command.SetArgs([]string{"delete", "preview"})
	if err := command.Execute(); err != nil {
		t.Fatalf("delete Execute() error = %v", err)
	}
	if deletedID != "preview" {
		t.Fatalf("deleted endpoint ID = %q", deletedID)
	}
}
