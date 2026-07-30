// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"testing"

	"gitea.dev/codespace/devcontainer"
)

func TestResolveConfiguredPorts(t *testing.T) {
	t.Parallel()

	appPorts := devcontainer.AppPortList{{Number: 3000}, {Address: "127.0.0.1:8080"}}
	for index, expected := range []uint16{3000, 8080} {
		resolved, err := appPorts[index].ContainerPort()
		if err != nil || resolved != expected {
			t.Fatalf("resolve appPort %d = %d, %v", index, resolved, err)
		}
	}
	forwarded, err := devContainerPort(devcontainer.Port{Address: "[::1]:9090"})
	if err != nil {
		t.Fatalf("resolve forwarded port: %v", err)
	}
	if forwarded != 9090 {
		t.Fatalf("forwardPorts = %d", forwarded)
	}
}
