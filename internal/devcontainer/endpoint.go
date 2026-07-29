// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

const (
	EndpointManifestPath     = "/var/lib/gitea-codespace/runtime/endpoints.json"
	EndpointManifestVersion  = 1
	MaxEndpointCount         = 64
	MaxDeclaredEndpointCount = MaxEndpointCount - 1
	WorkspaceEndpointID      = "workspace"
	WorkspaceEndpointLabel   = "Workspace"
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
