// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"gitea.dev/codespace/internal/runtimeplatform"
)

var runtimeBootStages = map[codespacev1.RuntimeBootStage]string{
	codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_RUNTIME:   "prepare-runtime",
	codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_INITIALIZE_SYSTEM: "initialize-system",
	codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_WORKSPACE: "prepare-workspace",
	codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_START_ENVIRONMENT: "start-environment",
	codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PUBLISH_READY:     "publish-ready",
	codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY:             "ready",
}

func runtimeBootFromProto(value *codespacev1.RuntimeBoot) (*api.RuntimeBoot, error) {
	if value == nil {
		return nil, nil
	}
	stage, ok := runtimeBootStages[value.Stage]
	if !ok {
		return nil, fmt.Errorf("invalid Runtime boot stage")
	}
	return &api.RuntimeBoot{OperationVersion: value.OperationRversion, Stage: stage, StartedUnix: value.StartedUnix, LastUpdateUnix: value.LastUpdateUnix}, nil
}

func runtimeBootToProto(value *api.RuntimeBoot) (*codespacev1.RuntimeBoot, error) {
	if value == nil {
		return nil, nil
	}
	var stage codespacev1.RuntimeBootStage
	for candidate, name := range runtimeBootStages {
		if name == value.Stage {
			stage = candidate
			break
		}
	}
	if stage == codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_UNSPECIFIED {
		return nil, fmt.Errorf("invalid persisted Runtime boot stage")
	}
	return &codespacev1.RuntimeBoot{OperationRversion: value.OperationVersion, Stage: stage, StartedUnix: value.StartedUnix, LastUpdateUnix: value.LastUpdateUnix}, nil
}

func runtimeMetadata(cs *api.Codespace) (*codespacev1.RuntimeMetadata, error) {
	boot, err := runtimeBootToProto(cs.Status.Boot)
	if err != nil {
		return nil, err
	}
	metadata := &codespacev1.RuntimeMetadata{Boot: boot}
	if cs.Status.Target != nil && cs.Status.Target.Ready {
		if !devcontainer.IsValidWorkspaceFolder(cs.Status.Target.WorkspaceFolder) {
			return nil, fmt.Errorf("ready Runtime target has an invalid workspace folder")
		}
		metadata.WorkspaceFolder = cs.Status.Target.WorkspaceFolder
		gateway, err := url.Parse(cs.Status.Access.GatewayURL)
		if err != nil || gateway.Host == "" {
			return nil, fmt.Errorf("runtime gateway address is unavailable")
		}
		alias := strings.ReplaceAll(string(cs.UID), "-", "")
		for _, endpoint := range cs.Status.Target.Endpoints {
			port := uint32(endpoint.Port)
			if endpoint.ID == runtimeendpoint.WorkspaceEndpointID {
				port = 0
			}
			host := alias + "." + gateway.Host
			if endpoint.ID != runtimeendpoint.WorkspaceEndpointID {
				host = endpoint.ID + "-" + host
			}
			address := &url.URL{Scheme: gateway.Scheme, Host: host, Path: "/"}
			open := *address
			open.Path = "/.gitea-codespace/open"
			metadata.Endpoints = append(metadata.Endpoints, &codespacev1.RuntimeEndpoint{EndpointId: endpoint.ID, Label: endpoint.Label, Port: port, Public: endpoint.Public, Url: address.String(), OpenUrl: open.String()})
		}
		host, port, err := net.SplitHostPort(cs.Status.Access.SSHAddress)
		if err != nil {
			return nil, fmt.Errorf("parse runtime SSH address: %w", err)
		}
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return nil, fmt.Errorf("invalid runtime SSH port")
		}
		user := runtimeplatform.SSHUserPrefix + string(cs.UID)
		remoteHost := host
		if number != 22 {
			remoteHost = net.JoinHostPort(host, port)
		}
		remote := user + "@" + remoteHost
		folder := strings.TrimPrefix(metadata.WorkspaceFolder, "/")
		metadata.Ssh = &codespacev1.RuntimeSSH{Host: host, Port: uint32(number), User: user,
			VscodeUrl: (&url.URL{Scheme: "vscode", Host: "vscode-remote", Path: "/ssh-remote+" + remote + "/" + folder}).String(),
			ZedUrl:    (&url.URL{Scheme: "zed", Host: "ssh", Path: "/" + remote + "/" + folder}).String(),
		}
	}
	return metadata, nil
}
