// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package controlplane

import "testing"

func TestCanonicalGiteaURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "HTTPS", raw: " https://gitea.example.com/ ", want: "https://gitea.example.com"},
		{name: "HTTP with path", raw: "http://gitea.example.com/sub/", want: "http://gitea.example.com/sub"},
		{name: "missing host", raw: "https:///gitea"},
		{name: "credentials", raw: "https://user@gitea.example.com"},
		{name: "query", raw: "https://gitea.example.com?redirect=1"},
		{name: "unsupported scheme", raw: "ssh://gitea.example.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := CanonicalGiteaURL(test.raw)
			if test.want == "" {
				if err == nil {
					t.Fatalf("CanonicalGiteaURL(%q) = %q, want an error", test.raw, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("CanonicalGiteaURL(%q) = %q, %v; want %q", test.raw, got, err, test.want)
			}
		})
	}
}
