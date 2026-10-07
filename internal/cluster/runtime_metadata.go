// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"fmt"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/runtimeendpoint"
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
		for _, endpoint := range cs.Status.Target.Endpoints {
			port := uint32(endpoint.Port)
			if endpoint.ID == runtimeendpoint.WorkspaceEndpointID {
				port = 0
			}
			metadata.Endpoints = append(metadata.Endpoints, &codespacev1.RuntimeEndpoint{EndpointId: endpoint.ID, Label: endpoint.Label, Port: port, Public: endpoint.Public})
		}
	}
	return metadata, nil
}
