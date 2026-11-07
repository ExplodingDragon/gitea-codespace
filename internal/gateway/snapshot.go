// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"fmt"
	"strings"
)

// A Runtime snapshot contains only the backend coordinates needed by Gateway.
type runtimeSnapshot struct {
	SiteUID       string
	ResourceUID   string
	RuntimeUUID   string
	GiteaWebURL   string
	PodUID        string
	AgentAddress  string
	TargetVersion int64
	Endpoints     []endpointSnapshot
}

// An endpoint snapshot carries one HTTP route without authorization data.
type endpointSnapshot struct {
	EndpointID string
	Label      string
	Public     bool
}

func validateRuntimeSnapshot(snapshot runtimeSnapshot) error {
	if strings.TrimSpace(snapshot.SiteUID) == "" || strings.TrimSpace(snapshot.ResourceUID) == "" ||
		strings.TrimSpace(snapshot.RuntimeUUID) == "" || strings.TrimSpace(snapshot.GiteaWebURL) == "" ||
		strings.TrimSpace(snapshot.PodUID) == "" || !validAgentAddress(snapshot.AgentAddress) || snapshot.TargetVersion <= 0 {
		return fmt.Errorf("gateway runtime snapshot is incomplete")
	}
	seen := make(map[string]struct{}, len(snapshot.Endpoints))
	for _, endpoint := range snapshot.Endpoints {
		if strings.TrimSpace(endpoint.EndpointID) == "" {
			return fmt.Errorf("gateway endpoint snapshot is incomplete")
		}
		if _, ok := seen[endpoint.EndpointID]; ok {
			return fmt.Errorf("gateway endpoint %q is duplicated", endpoint.EndpointID)
		}
		seen[endpoint.EndpointID] = struct{}{}
	}
	return nil
}
