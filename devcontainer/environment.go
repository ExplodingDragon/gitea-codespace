// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import "maps"

// ProxyVariableNames returns the conventional upper- and lower-case
// proxy variable names understood by Docker and common development tools.
func ProxyVariableNames() []string {
	return []string{
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	}
}

// ProcessEnvironment combines the resolved remote environment with values that
// apply to one process. Later overlays take precedence.
func ProcessEnvironment(remote map[string]string, overlays ...map[string]string) map[string]string {
	length := len(remote)
	for _, overlay := range overlays {
		length += len(overlay)
	}
	result := make(map[string]string, length)
	maps.Copy(result, remote)
	for _, overlay := range overlays {
		maps.Copy(result, overlay)
	}
	return result
}
