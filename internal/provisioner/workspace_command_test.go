// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import "testing"

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
