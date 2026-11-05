// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package controlplane

import (
	"fmt"
	"net/url"
	"strings"
)

// CanonicalGiteaURL validates a Gitea URL and removes surrounding whitespace
// and trailing slashes so every control-plane caller compares the same value.
func CanonicalGiteaURL(raw string) (string, error) {
	canonical := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(canonical)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid Gitea URL")
	}
	return canonical, nil
}
