// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"fmt"
	"strings"
)

// RuntimeSnapshot contains only the backend coordinates needed to reach one runtime.
type RuntimeSnapshot struct {
	SiteUID       string             `json:"site_uid"`
	ResourceUID   string             `json:"resource_uid"`
	RuntimeUUID   string             `json:"runtime_uuid"`
	GiteaWebURL   string             `json:"gitea_web_url"`
	PodUID        string             `json:"pod_uid"`
	AgentAddress  string             `json:"agent_address"`
	TargetVersion int64              `json:"target_version"`
	Endpoints     []EndpointSnapshot `json:"endpoints"`
}

// EndpointSnapshot contains one HTTP endpoint route without authorization data.
type EndpointSnapshot struct {
	EndpointID string `json:"endpoint_id"`
	Label      string `json:"label"`
	Public     bool   `json:"public"`
}

func validateRuntimeSnapshot(snapshot RuntimeSnapshot) error {
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
