// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

// ConfiguredEndpoints returns the repository defaults used for the first create.
// The Agent persists the returned values before publishing a ready target.
func ConfiguredEndpoints(state *devcontainer.State) ([]*codespacev1.RuntimeEndpoint, map[string]string, error) {
	if err := state.Validate(); err != nil {
		return nil, nil, err
	}
	type configuredPort struct {
		endpointID  string
		address     string
		service     string
		containerID string
		port        uint16
	}
	ports := map[string]configuredPort{}
	for _, configured := range state.Configuration.ForwardPorts {
		service, port, err := devContainerPort(configured)
		if err != nil {
			return nil, nil, fmt.Errorf("forwardPorts: %w", err)
		}
		if port == 0 {
			continue
		}
		containerID := state.PrimaryContainerID
		endpointID := runtimeendpoint.PortEndpointID(port)
		if service != "" {
			if state.ComposeProject == "" {
				return nil, nil, fmt.Errorf("forwardPorts service %q requires Docker Compose", service)
			}
			containerID = state.ComposeContainers[service]
			if containerID == "" {
				return nil, nil, fmt.Errorf("forwardPorts service %q is not running", service)
			}
			endpointID = runtimeendpoint.ServicePortEndpointID(service, port)
		}
		ports[endpointID] = configuredPort{endpointID: endpointID, address: configured.Address, service: service, containerID: containerID, port: port}
	}
	for _, configured := range state.Configuration.AppPort {
		port, err := configured.ContainerPort()
		if err != nil {
			return nil, nil, fmt.Errorf("appPort: %w", err)
		}
		if port != 0 {
			endpointID := runtimeendpoint.PortEndpointID(port)
			ports[endpointID] = configuredPort{endpointID: endpointID, address: configured.Address, containerID: state.PrimaryContainerID, port: port}
		}
	}
	endpoints := make([]*codespacev1.RuntimeEndpoint, 0, len(ports))
	ordered := make([]configuredPort, 0, len(ports))
	for _, port := range ports {
		ordered = append(ordered, port)
	}
	slices.SortFunc(ordered, func(left, right configuredPort) int {
		if left.port != right.port {
			return int(left.port) - int(right.port)
		}
		return strings.Compare(left.endpointID, right.endpointID)
	})
	targets := make(map[string]string, len(ordered))
	for _, configured := range ordered {
		attributes := devcontainer.PortAttributesForAddress(state.Configuration, configured.address, configured.port)
		if attributes.OnAutoForward == "ignore" {
			continue
		}
		label := strings.TrimSpace(attributes.Label)
		if label == "" {
			if configured.service == "" {
				label = "Port " + strconv.Itoa(int(configured.port))
			} else {
				label = configured.service + ":" + strconv.Itoa(int(configured.port))
			}
		}
		if err := runtimeendpoint.ValidateLabel(label); err != nil {
			return nil, nil, fmt.Errorf("port %d label: %w", configured.port, err)
		}
		endpoints = append(endpoints, &codespacev1.RuntimeEndpoint{EndpointId: configured.endpointID, Label: label, Port: uint32(configured.port)})
		targets[configured.endpointID] = configured.containerID
	}
	if len(endpoints) > runtimeendpoint.MaxDeclaredEndpointCount {
		return nil, nil, fmt.Errorf("configured endpoints exceed limit %d", runtimeendpoint.MaxDeclaredEndpointCount)
	}
	return endpoints, targets, nil
}

func devContainerPort(port devcontainer.Port) (string, uint16, error) {
	if port.Numeric || port.Number != 0 {
		return "", port.Number, nil
	}
	host, rawPort, err := net.SplitHostPort(port.Address)
	if err != nil {
		return "", 0, fmt.Errorf("port %q must use host:port", port.Address)
	}
	service := host
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		service = ""
	}
	value, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || value == 0 {
		return "", 0, fmt.Errorf("port %q is invalid", port.Address)
	}
	return service, uint16(value), nil
}
