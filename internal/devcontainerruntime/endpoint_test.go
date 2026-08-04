// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"testing"

	"gitea.dev/codespace/devcontainer"
)

func TestDevContainerPortAcceptsLoopbackForwardPort(t *testing.T) {
	t.Parallel()

	forwarded, err := devContainerPort(devcontainer.Port{Address: "[::1]:9090"})
	if err != nil {
		t.Fatalf("resolve forwarded port: %v", err)
	}
	if forwarded != 9090 {
		t.Fatalf("forwardPorts = %d", forwarded)
	}
}
