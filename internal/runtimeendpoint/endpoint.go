// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimeendpoint

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	EndpointManifestPath     = "/var/lib/gitea-codespace/runtime/endpoints.json"
	EndpointManifestVersion  = 1
	MaxEndpointCount         = 64
	MaxDeclaredEndpointCount = MaxEndpointCount - 1
	WorkspaceEndpointID      = "workspace"
	WorkspaceEndpointLabel   = "Workspace"
	WorkspaceEndpointPort    = 13337
)

// EndpointManifest is the runtime-owned list of ordinary HTTP endpoints published through the Gateway.
type EndpointManifest struct {
	Version   int        `json:"version"`
	Endpoints []Endpoint `json:"endpoints"`
}

// Endpoint identifies one localhost service in the primary Dev Container.
type Endpoint struct {
	EndpointID     string `json:"endpoint_id"`
	Label          string `json:"label"`
	UpstreamScheme string `json:"upstream_scheme"`
	UpstreamPort   int    `json:"upstream_port"`
	Public         bool   `json:"public"`
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
