// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimeendpoint

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	AgentSocketPath          = "/run/codespace/endpoint.sock"
	ContainerSocketPath      = "/var/run/gitea-codespace/endpoint.sock"
	MaxEndpointCount         = 64
	MaxDeclaredEndpointCount = MaxEndpointCount - 1
	WorkspaceEndpointID      = "workspace"
	WorkspaceEndpointLabel   = "Workspace"
	WorkspaceEndpointPort    = 13337
)

func PortEndpointID(port uint16) string {
	return "port-" + strconv.Itoa(int(port))
}

// ValidateLabel applies the common label constraints used by runtime declarations and Gateway routes.
func ValidateLabel(label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("endpoint label is required")
	}
	if !utf8.ValidString(label) {
		return fmt.Errorf("endpoint label must be valid UTF-8")
	}
	if utf8.RuneCountInString(label) > 64 {
		return fmt.Errorf("endpoint label is too long")
	}
	if strings.ContainsFunc(label, func(r rune) bool { return unicode.IsControl(r) || r == '<' || r == '>' }) {
		return fmt.Errorf("endpoint label contains an invalid character")
	}
	return nil
}
