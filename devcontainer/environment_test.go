// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"slices"
	"testing"
)

func TestProcessEnvironmentPrecedence(t *testing.T) {
	t.Parallel()

	remote := map[string]string{"REMOTE": "remote", "SHARED": "remote"}
	secrets := map[string]string{"SECRET": "secret", "SHARED": "secret"}
	platform := map[string]string{"PLATFORM": "platform", "SHARED": "platform"}
	result := ProcessEnvironment(remote, secrets, platform)

	if result["REMOTE"] != "remote" || result["SECRET"] != "secret" || result["PLATFORM"] != "platform" || result["SHARED"] != "platform" {
		t.Fatalf("process environment = %#v", result)
	}
	if remote["SHARED"] != "remote" || secrets["SHARED"] != "secret" {
		t.Fatalf("source environments were modified")
	}
}

func TestEnvironmentList(t *testing.T) {
	t.Parallel()

	if list := EnvironmentList(map[string]string{"B": "2", "A": "1"}); !slices.Equal(list, []string{"A=1", "B=2"}) {
		t.Fatalf("environment list = %#v", list)
	}
}
