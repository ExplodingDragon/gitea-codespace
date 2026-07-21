// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"strings"
	"testing"
)

func TestSanitizeGatewayReturnTo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "path and query", in: "/w/?folder=%2Fworkspace", want: "/w/?folder=%2Fworkspace"},
		{name: "external url", in: "https://example.test/w/", want: "/"},
		{name: "network path", in: "//example.test/w/", want: "/"},
		{name: "backslash", in: `/w\path`, want: "/"},
		{name: "reserved root path", in: "/.gitea-codespace/open", want: "/"},
		{name: "reserved nested path", in: "/w/.gitea-codespace/open", want: "/"},
		{name: "control character", in: "/w/\x1f", want: "/"},
		{name: "too long", in: "/" + strings.Repeat("a", gatewayReturnToMaxBytes), want: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeGatewayReturnTo(tt.in); got != tt.want {
				t.Fatalf("sanitize return_to = %q, want %q", got, tt.want)
			}
		})
	}
}
