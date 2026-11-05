// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package runtimeendpoint defines the endpoint contract shared by Agent, Gateway, and injected commands.
package runtimeendpoint

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// AgentSocketPath is the endpoint socket owned by Agent.
const AgentSocketPath = "/run/codespace/endpoint.sock"

// ContainerSocketPath is the socket's read-write bind mount in the Dev Container.
const ContainerSocketPath = "/var/run/gitea-codespace/endpoint.sock"

// MaxEndpointCount includes the platform-managed workspace endpoint.
const MaxEndpointCount = 64

// MaxDeclaredEndpointCount is the number of endpoints a user can add.
const MaxDeclaredEndpointCount = MaxEndpointCount - 1

// WorkspaceEndpointID identifies the platform-managed Web IDE endpoint.
const WorkspaceEndpointID = "workspace"

// WorkspaceEndpointLabel is the user-facing label for the Web IDE endpoint.
const WorkspaceEndpointLabel = "Workspace"

// WorkspaceEndpointPort is the fixed Web IDE port inside the Dev Container.
const WorkspaceEndpointPort = 13337

// PortEndpointID returns the stable identifier for a user-declared port.
func PortEndpointID(port uint16) string {
	return "port-" + strconv.Itoa(int(port))
}

// ServicePortEndpointID identifies a Compose service port without exposing the service name.
func ServicePortEndpointID(service string, port uint16) string {
	digest := sha256.Sum256([]byte(service))
	return fmt.Sprintf("port-%d-%x", port, digest[:8])
}

// IsPortEndpointID verifies an endpoint ID generated for a declared or dynamic port.
func IsPortEndpointID(endpointID string, port uint16) bool {
	if endpointID == PortEndpointID(port) {
		return true
	}
	prefix := PortEndpointID(port) + "-"
	if len(endpointID) != len(prefix)+16 || !strings.HasPrefix(endpointID, prefix) {
		return false
	}
	for _, char := range endpointID[len(prefix):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
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
