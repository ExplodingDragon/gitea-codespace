// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import "testing"

func TestWorkspaceCommandEnvironmentSetsTerminalForInteractivePTY(t *testing.T) {
	t.Parallel()

	environment := workspaceCommandEnvironment(true)
	if environment["TERM"] != "xterm-256color" {
		t.Fatalf("TERM = %q", environment["TERM"])
	}
	if environment["COLORTERM"] != "truecolor" {
		t.Fatalf("COLORTERM = %q", environment["COLORTERM"])
	}
}

func TestWorkspaceCommandEnvironmentKeepsNonInteractiveClean(t *testing.T) {
	t.Parallel()

	if environment := workspaceCommandEnvironment(false); environment != nil {
		t.Fatalf("non-interactive environment = %#v", environment)
	}
}

func TestWorkspaceCommandResizeRequiresControlSocket(t *testing.T) {
	t.Parallel()

	session := &incusWorkspaceCommandSession{
		controlReady: make(chan struct{}),
	}
	session.setControl(nil)
	if err := session.Resize(120, 40); err == nil {
		t.Fatalf("resize without control socket succeeded")
	}
}
