// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"testing"

	"gitea.dev/codespace/internal/devcontainer"
)

func TestCollectConfiguredPorts(t *testing.T) {
	t.Parallel()

	ports := map[uint16]struct{}{}
	if err := collectAppPorts([]any{float64(3000), "127.0.0.1:8080"}, ports); err != nil {
		t.Fatalf("collect app ports: %v", err)
	}
	forwarded, err := devContainerPort(devcontainer.Port{Address: "[::1]:9090"})
	if err != nil {
		t.Fatalf("resolve forwarded port: %v", err)
	}
	ports[forwarded] = struct{}{}
	if _, ok := ports[3000]; !ok {
		t.Fatalf("appPort 3000 was not collected: %#v", ports)
	}
	if _, ok := ports[8080]; !ok {
		t.Fatalf("appPort 8080 was not collected: %#v", ports)
	}
	if _, ok := ports[9090]; !ok {
		t.Fatalf("forwardPorts 9090 was not collected: %#v", ports)
	}
}
