// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package webide

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseImageReference(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct{ input, want string }{
		{"ide", "docker.io/library/ide:latest"},
		{"team/ide:1.0", "docker.io/team/ide:1.0"},
		{"docker.io/ide", "docker.io/library/ide:latest"},
		{"index.docker.io/team/ide", "docker.io/team/ide:latest"},
		{"registry.example/ide", "registry.example/ide:latest"},
		{" registry.example/ide:latest ", "registry.example/ide:latest"},
		{"registry.example/ide:0.2.2-rc.3", "registry.example/ide:0.2.2-rc.3"},
		{"localhost:5000/team/ide", "localhost:5000/team/ide:latest"},
		{"localhost/ide", "localhost/ide:latest"},
		{"registry:5000/team/ide", "registry:5000/team/ide:latest"},
		{"127.0.0.1:5000/team/ide:_RC.1", "127.0.0.1:5000/team/ide:_RC.1"},
		{"Registry.EXAMPLE/team/ide:V1", "Registry.EXAMPLE/team/ide:V1"},
		{"registry.example/team/tools/ide__test--build:v1", "registry.example/team/tools/ide__test--build:v1"},
		{"[2001:db8::1]:5000/team/ide:v1", "[2001:db8::1]:5000/team/ide:v1"},
		{"registry.example/ide@" + digest, "registry.example/ide@" + digest},
		{"registry.example/ide:v1@" + digest, "registry.example/ide@" + digest},
		{"team/ide:v1@" + digest, "docker.io/team/ide@" + digest},
	} {
		t.Run(test.input, func(t *testing.T) {
			reference, err := ParseImageReference(test.input)
			require.NoError(t, err)
			require.Equal(t, test.want, reference.String())
		})
	}
	for _, input := range []string{"", "https://registry.example/ide", "oci://registry.example/ide", "registry.example/ide:", "registry.example/ide@sha256:abc", "registry.example/Team/ide", "registry.example/ide:1.0+build"} {
		t.Run(input, func(t *testing.T) {
			_, err := ParseImageReference(input)
			require.Error(t, err)
		})
	}
}
