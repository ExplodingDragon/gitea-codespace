// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

// ConfiguredEndpoints returns the repository defaults used for the first create.
// The Agent persists the returned values before publishing a ready target.
func ConfiguredEndpoints(configuration devcontainer.Configuration) ([]*codespacev1.RuntimeEndpoint, error) {
	ports := map[uint16]struct{}{}
	for _, port := range configuration.ForwardPorts {
		value, err := devContainerPort(port)
		if err != nil {
			return nil, devcontainer.InvalidConfiguration(fmt.Errorf("forwardPorts: %w", err))
		}
		if value != 0 {
			ports[value] = struct{}{}
		}
	}
	for _, port := range configuration.AppPort {
		value, err := port.ContainerPort()
		if err != nil {
			return nil, devcontainer.InvalidConfiguration(fmt.Errorf("appPort: %w", err))
		}
		if value != 0 {
			ports[value] = struct{}{}
		}
	}
	endpoints := make([]*codespacev1.RuntimeEndpoint, 0, len(ports))
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
		label := strings.TrimSpace(attributes.Label)
		if label == "" {
			label = "Port " + strconv.Itoa(int(port))
		}
		if err := runtimeendpoint.ValidateLabel(label); err != nil {
			return nil, devcontainer.InvalidConfiguration(fmt.Errorf("port %d label: %w", port, err))
		}
		endpoints = append(endpoints, &codespacev1.RuntimeEndpoint{EndpointId: runtimeendpoint.PortEndpointID(port), Label: label, Port: uint32(port)})
	}
	if len(endpoints) > runtimeendpoint.MaxDeclaredEndpointCount {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("configured endpoints exceed limit %d", runtimeendpoint.MaxDeclaredEndpointCount))
	}
	return endpoints, nil
}

func devContainerPort(port devcontainer.Port) (uint16, error) {
	if port.Numeric || port.Number != 0 {
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
