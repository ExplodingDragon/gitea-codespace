// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

var endpointIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,28}[a-z0-9])?$`)

// SetEndpoint adds or replaces a runtime endpoint declaration.
func SetEndpoint(id, label, scheme string, port uint16, public bool) error {
	if err := validateEndpointID(id); err != nil {
		return err
	}
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("endpoint scheme must be http or https")
	}
	label = strings.TrimSpace(label)
	if err := runtimeendpoint.ValidateLabel(label); err != nil {
		return fmt.Errorf("endpoint label is invalid")
	}
	if port == 0 {
		return fmt.Errorf("endpoint port is invalid")
	}
	manifest, err := readEndpointManifest()
	if err != nil {
		return err
	}
	manifest.Endpoints = slices.DeleteFunc(manifest.Endpoints, func(endpoint runtimeendpoint.Endpoint) bool {
		return endpoint.EndpointID == id
	})
	manifest.Endpoints = append(manifest.Endpoints, runtimeendpoint.Endpoint{
		EndpointID: id, Label: label, UpstreamScheme: scheme, UpstreamPort: int(port), Public: public,
	})
	return writeEndpointManifest(manifest)
}

// DeleteEndpoint removes a runtime endpoint declaration.
func DeleteEndpoint(id string) error {
	if err := validateEndpointID(id); err != nil {
		return err
	}
	manifest, err := readEndpointManifest()
	if err != nil {
		return err
	}
	manifest.Endpoints = slices.DeleteFunc(manifest.Endpoints, func(endpoint runtimeendpoint.Endpoint) bool {
		return endpoint.EndpointID == id
	})
	return writeEndpointManifest(manifest)
}

func validateEndpointID(id string) error {
	if id == runtimeendpoint.WorkspaceEndpointID || !endpointIDPattern.MatchString(id) {
		return fmt.Errorf("endpoint id %q is invalid", id)
	}
	return nil
}

func readEndpointManifest() (runtimeendpoint.EndpointManifest, error) {
	manifest := runtimeendpoint.EndpointManifest{Version: runtimeendpoint.EndpointManifestVersion}
	if err := readJSON(runtimeendpoint.EndpointManifestPath, &manifest); err != nil && !os.IsNotExist(err) {
		return manifest, fmt.Errorf("read endpoint manifest: %w", err)
	}
	if manifest.Version != runtimeendpoint.EndpointManifestVersion {
		return manifest, fmt.Errorf("endpoint manifest version %d is invalid", manifest.Version)
	}
	return manifest, nil
}

func writeEndpointManifest(manifest runtimeendpoint.EndpointManifest) error {
	if len(manifest.Endpoints) > runtimeendpoint.MaxDeclaredEndpointCount {
		return fmt.Errorf("endpoint manifest exceeds limit %d", runtimeendpoint.MaxDeclaredEndpointCount)
	}
	slices.SortFunc(manifest.Endpoints, func(a, b runtimeendpoint.Endpoint) int {
		if a.EndpointID < b.EndpointID {
			return -1
		}
		if a.EndpointID > b.EndpointID {
			return 1
		}
		return 0
	})
	return writeJSONAtomic(runtimeendpoint.EndpointManifestPath, manifest)
}
