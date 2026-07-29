// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

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

	"gitea.dev/codespace/internal/devcontainer"
)

func writeConfiguredEndpoints(configuration devcontainer.Configuration) error {
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
	if len(ports) > devcontainer.MaxDeclaredEndpointCount {
		return fmt.Errorf("configured endpoints exceed limit %d", devcontainer.MaxDeclaredEndpointCount)
	}
	manifest := devcontainer.EndpointManifest{Version: devcontainer.EndpointManifestVersion, Endpoints: make([]devcontainer.Endpoint, 0, len(ports))}
	ordered := make([]int, 0, len(ports))
	for port := range ports {
		ordered = append(ordered, int(port))
	}
	sort.Ints(ordered)
	for _, rawPort := range ordered {
		port := uint16(rawPort)
		attributes := configuration.PortsAttributes[strconv.Itoa(int(port))]
		scheme := strings.ToLower(strings.TrimSpace(attributes.Protocol))
		if scheme == "" {
			scheme = "http"
		}
		label := strings.TrimSpace(attributes.Label)
		if label == "" {
			label = "Port " + strconv.Itoa(int(port))
		}
		manifest.Endpoints = append(manifest.Endpoints, devcontainer.Endpoint{
			EndpointID:     "port-" + strconv.Itoa(int(port)),
			Label:          label,
			UpstreamScheme: scheme,
			UpstreamPort:   int(port),
		})
	}
	return writeEndpointManifest(manifest)
}

func writeEndpointManifest(manifest devcontainer.EndpointManifest) error {
	directory := filepath.Dir(devcontainer.EndpointManifestPath)
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
	return os.Rename(temporary, devcontainer.EndpointManifestPath)
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
