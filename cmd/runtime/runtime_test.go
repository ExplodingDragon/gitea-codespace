// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"testing"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

func TestEndpointCommandsMapArguments(t *testing.T) {
	var gotLabel string
	var gotPort uint16
	var gotPublic bool
	list := func(context.Context) ([]*codespacev1.RuntimeEndpoint, error) { return nil, nil }
	command := newEndpointCommandWithRun(list, func(_ context.Context, port uint16, label string, public bool) error {
		gotLabel, gotPort, gotPublic = label, port, public
		return nil
	}, func(context.Context, uint16) error {
		t.Fatal("delete called while testing set")
		return nil
	})
	command.SetArgs([]string{"set", "8443", "--label", "Web preview", "--public"})
	if err := command.Execute(); err != nil {
		t.Fatalf("set Execute() error = %v", err)
	}
	if gotLabel != "Web preview" || gotPort != 8443 || !gotPublic {
		t.Fatalf("set arguments = (%q, %d, %t)", gotLabel, gotPort, gotPublic)
	}

	var deletedPort uint16
	command = newEndpointCommandWithRun(list, func(context.Context, uint16, string, bool) error {
		t.Fatal("set called while testing delete")
		return nil
	}, func(_ context.Context, port uint16) error {
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
