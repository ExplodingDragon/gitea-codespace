// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

func WriteConfiguredEndpoints(configuration devcontainer.Configuration) error {
	ports := map[uint16]struct{}{}
	for _, port := range configuration.ForwardPorts {
		value, err := devContainerPort(port)
		if err != nil {
			return fmt.Errorf("forwardPorts: %w", err)
		}
		ports[value] = struct{}{}
	}
	if len(configuration.AppPort) > 0 && string(configuration.AppPort) != "null" {
		var values any
		if err := json.Unmarshal(configuration.AppPort, &values); err != nil {
			return fmt.Errorf("appPort: %w", err)
		}
		if err := collectAppPorts(values, ports); err != nil {
			return err
		}
	}
	manifest := runtimeendpoint.EndpointManifest{Version: runtimeendpoint.EndpointManifestVersion, Endpoints: make([]runtimeendpoint.Endpoint, 0, len(ports))}
	ordered := make([]int, 0, len(ports))
	for port := range ports {
		ordered = append(ordered, int(port))
	}
	sort.Ints(ordered)
	for _, rawPort := range ordered {
		port := uint16(rawPort)
		attributes := devcontainer.PortAttributesFor(configuration, port)
		if attributes.OnAutoForward == "ignore" {
			continue
		}
		scheme := strings.ToLower(strings.TrimSpace(attributes.Protocol))
		if scheme == "" {
			scheme = "http"
		}
		label := strings.TrimSpace(attributes.Label)
		if label == "" {
			label = "Port " + strconv.Itoa(int(port))
		}
		if err := runtimeendpoint.ValidateLabel(label); err != nil {
			return devcontainer.InvalidConfiguration(fmt.Errorf("port %d label: %w", port, err))
		}
		manifest.Endpoints = append(manifest.Endpoints, runtimeendpoint.Endpoint{
			EndpointID:     "port-" + strconv.Itoa(int(port)),
			Label:          label,
			UpstreamScheme: scheme,
			UpstreamPort:   int(port),
		})
	}
	if len(manifest.Endpoints) > runtimeendpoint.MaxDeclaredEndpointCount {
		return devcontainer.InvalidConfiguration(fmt.Errorf("configured endpoints exceed limit %d", runtimeendpoint.MaxDeclaredEndpointCount))
	}
	return writeEndpointManifest(manifest)
}

func writeEndpointManifest(manifest runtimeendpoint.EndpointManifest) error {
	directory := filepath.Dir(runtimeendpoint.EndpointManifestPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".endpoints-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	encodeErr := json.NewEncoder(file).Encode(manifest)
	chmodErr := file.Chmod(0o600)
	closeErr := file.Close()
	if err := errors.Join(encodeErr, chmodErr, closeErr); err != nil {
		return err
	}
	return os.Rename(temporary, runtimeendpoint.EndpointManifestPath)
}

func devContainerPort(port devcontainer.Port) (uint16, error) {
	if port.Number != 0 {
		return port.Number, nil
	}
	host, rawPort, err := net.SplitHostPort(port.Address)
	if err != nil {
		return 0, fmt.Errorf("port %q must use host:port", port.Address)
	}
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return 0, fmt.Errorf("port %q does not target the primary Dev Container localhost", port.Address)
	}
	value, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("port %q is invalid", port.Address)
	}
	return uint16(value), nil
}

func collectAppPorts(value any, ports map[uint16]struct{}) error {
	switch value := value.(type) {
	case float64:
		if value < 1 || value > 65535 || value != float64(uint16(value)) {
			return fmt.Errorf("appPort number is invalid")
		}
		ports[uint16(value)] = struct{}{}
	case string:
		parts := strings.Split(value, ":")
		raw := parts[len(parts)-1]
		port, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || port == 0 {
			return fmt.Errorf("appPort %q is invalid", value)
		}
		ports[uint16(port)] = struct{}{}
	case []any:
		for _, item := range value {
			if err := collectAppPorts(item, ports); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("appPort must be a port or array of ports")
	}
	return nil
}
