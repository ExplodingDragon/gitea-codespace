// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

// SetEndpoint adds or replaces a runtime endpoint declaration.
func SetEndpoint(port uint16, label, scheme string, public bool) error {
	if port == 0 {
		return fmt.Errorf("endpoint port is invalid")
	}
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("endpoint scheme must be http or https")
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "Port " + strconv.Itoa(int(port))
	}
	if err := runtimeendpoint.ValidateLabel(label); err != nil {
		return fmt.Errorf("endpoint label is invalid")
	}
	id := endpointID(port)
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
func DeleteEndpoint(port uint16) error {
	if port == 0 {
		return fmt.Errorf("endpoint port is invalid")
	}
	id := endpointID(port)
	manifest, err := readEndpointManifest()
	if err != nil {
		return err
	}
	manifest.Endpoints = slices.DeleteFunc(manifest.Endpoints, func(endpoint runtimeendpoint.Endpoint) bool {
		return endpoint.EndpointID == id
	})
	return writeEndpointManifest(manifest)
}

// ListEndpoints returns the current runtime declarations ordered by port.
func ListEndpoints() ([]runtimeendpoint.Endpoint, error) {
	manifest, err := readEndpointManifest()
	if err != nil {
		return nil, err
	}
	sortEndpoints(manifest.Endpoints)
	return manifest.Endpoints, nil
}

func endpointID(port uint16) string {
	return "port-" + strconv.Itoa(int(port))
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
	sortEndpoints(manifest.Endpoints)
	return writeJSONAtomic(runtimeendpoint.EndpointManifestPath, manifest)
}

func sortEndpoints(endpoints []runtimeendpoint.Endpoint) {
	slices.SortFunc(endpoints, func(a, b runtimeendpoint.Endpoint) int {
		if a.UpstreamPort < b.UpstreamPort {
			return -1
		}
		if a.UpstreamPort > b.UpstreamPort {
			return 1
		}
		return strings.Compare(a.EndpointID, b.EndpointID)
	})
}
