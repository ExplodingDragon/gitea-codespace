// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	"gitea.dev/codespace-proto-go/agent/v1/agentv1connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

func endpointClient() (agentv1connect.RuntimeEndpointServiceClient, *http.Client) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", runtimeendpoint.ContainerSocketPath)
	}}
	client := &http.Client{Transport: transport}
	return agentv1connect.NewRuntimeEndpointServiceClient(client, "http://runtime-agent"), client
}

// SetEndpoint adds or replaces a runtime endpoint declaration.
func SetEndpoint(ctx context.Context, port uint16, label string, public bool) error {
	if port == 0 {
		return fmt.Errorf("endpoint port is invalid")
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "Port " + strconv.Itoa(int(port))
	}
	if err := runtimeendpoint.ValidateLabel(label); err != nil {
		return fmt.Errorf("endpoint label is invalid")
	}
	remote, client := endpointClient()
	defer client.CloseIdleConnections()
	_, err := remote.Set(ctx, connect.NewRequest(&agentv1.RuntimeEndpointServiceSetRequest{
		ProtocolVersion: 1,
		Endpoint:        &codespacev1.RuntimeEndpoint{EndpointId: runtimeendpoint.PortEndpointID(port), Label: label, Public: public, Port: uint32(port)},
	}))
	return err
}

// DeleteEndpoint removes a runtime endpoint declaration.
func DeleteEndpoint(ctx context.Context, port uint16) error {
	if port == 0 {
		return fmt.Errorf("endpoint port is invalid")
	}
	remote, client := endpointClient()
	defer client.CloseIdleConnections()
	_, err := remote.Delete(ctx, connect.NewRequest(&agentv1.RuntimeEndpointServiceDeleteRequest{ProtocolVersion: 1, EndpointId: runtimeendpoint.PortEndpointID(port)}))
	return err
}

// ListEndpoints returns the current runtime declarations ordered by port.
func ListEndpoints(ctx context.Context) ([]*codespacev1.RuntimeEndpoint, error) {
	remote, client := endpointClient()
	defer client.CloseIdleConnections()
	response, err := remote.List(ctx, connect.NewRequest(&agentv1.RuntimeEndpointServiceListRequest{ProtocolVersion: 1}))
	if err != nil {
		return nil, err
	}
	return response.Msg.Endpoints, nil
}
