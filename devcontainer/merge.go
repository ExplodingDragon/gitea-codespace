// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	dockerunits "github.com/docker/go-units"
)

// Merge applies the Dev Container metadata merge rules with override as the later source.
func Merge(base, override Configuration) Configuration {
	result := base
	if override.Schema != "" {
		result.Schema = override.Schema
	}
	if override.Name != "" {
		result.Name = override.Name
	}
	if override.Image != "" {
		result.Image = override.Image
	}
	if override.Build != nil {
		result.Build = override.Build
	}
	if override.DockerFile != "" {
		result.DockerFile = override.DockerFile
	}
	if override.Context != "" {
		result.Context = override.Context
	}
	if len(override.DockerComposeFile) > 0 {
		result.DockerComposeFile = slices.Clone(override.DockerComposeFile)
	}
	if override.Service != "" {
		result.Service = override.Service
	}
	if len(override.RunServices) > 0 {
		result.RunServices = appendUnique(result.RunServices, override.RunServices)
	}
	if override.WorkspaceFolder != "" {
		result.WorkspaceFolder = override.WorkspaceFolder
	}
	if override.WorkspaceMount != "" {
		result.WorkspaceMount = override.WorkspaceMount
	}
	result.Mounts = mergeMounts(result.Mounts, override.Mounts)
	result.ContainerEnv = mergeMap(result.ContainerEnv, override.ContainerEnv)
	result.RemoteEnv = mergeRemoteEnvironment(result.RemoteEnv, override.RemoteEnv)
	if override.ContainerUser != "" {
		result.ContainerUser = override.ContainerUser
	}
	if override.RemoteUser != "" {
		result.RemoteUser = override.RemoteUser
	}
	if override.UpdateRemoteUserUID != nil {
		result.UpdateRemoteUserUID = override.UpdateRemoteUserUID
	}
	if override.UserEnvProbe != "" {
		result.UserEnvProbe = override.UserEnvProbe
	}
	result.InitializeCommand = mergeCommand(result.InitializeCommand, override.InitializeCommand, "configuration")
	result.OnCreateCommand = mergeCommand(result.OnCreateCommand, override.OnCreateCommand, "configuration")
	result.UpdateContentCommand = mergeCommand(result.UpdateContentCommand, override.UpdateContentCommand, "configuration")
	result.PostCreateCommand = mergeCommand(result.PostCreateCommand, override.PostCreateCommand, "configuration")
	result.PostStartCommand = mergeCommand(result.PostStartCommand, override.PostStartCommand, "configuration")
	result.PostAttachCommand = mergeCommand(result.PostAttachCommand, override.PostAttachCommand, "configuration")
	if override.WaitFor != "" {
		result.WaitFor = override.WaitFor
	}
	if override.ShutdownAction != "" {
		result.ShutdownAction = override.ShutdownAction
	}
	result.Features = mergeRawMap(result.Features, override.Features)
	if len(override.OverrideFeatureInstallOrder) > 0 {
		result.OverrideFeatureInstallOrder = slices.Clone(override.OverrideFeatureInstallOrder)
	}
	result.Customizations = mergeCustomizations(result.Customizations, override.Customizations)
	result.ForwardPorts = appendUniqueComparable(result.ForwardPorts, override.ForwardPorts)
	result.PortsAttributes = mergeStructMap(result.PortsAttributes, override.PortsAttributes)
	if override.OtherPortsAttributes != nil {
		attributes := *override.OtherPortsAttributes
		result.OtherPortsAttributes = &attributes
	}
	if len(override.AppPort) > 0 {
		result.AppPort = slices.Clone(override.AppPort)
	}
	if len(override.RunArgs) > 0 {
		result.RunArgs = slices.Clone(override.RunArgs)
	}
	result.Init = result.Init || override.Init
	result.Privileged = result.Privileged || override.Privileged
	result.CapAdd = appendUnique(result.CapAdd, override.CapAdd)
	result.SecurityOpt = appendUnique(result.SecurityOpt, override.SecurityOpt)
	if override.OverrideCommand != nil {
		result.OverrideCommand = override.OverrideCommand
	}
	result.HostRequirements = mergeHostRequirements(result.HostRequirements, override.HostRequirements)
	result.Secrets = mergeStructMap(result.Secrets, override.Secrets)
	return result
}

func mergeHostRequirements(base, override HostRequirements) HostRequirements {
	result := base
	if override.CPUs > result.CPUs {
		result.CPUs = override.CPUs
	}
	result.Memory = largerByteRequirement(result.Memory, override.Memory)
	result.Storage = largerByteRequirement(result.Storage, override.Storage)
	if gpuRequirementRank(override.GPU) > gpuRequirementRank(result.GPU) {
		result.GPU = slices.Clone(override.GPU)
	}
	return result
}

func largerByteRequirement(base, override string) string {
	if strings.TrimSpace(override) == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return override
	}
	baseBytes, baseErr := dockerunits.RAMInBytes(base)
	overrideBytes, overrideErr := dockerunits.RAMInBytes(override)
	if baseErr != nil {
		return base
	}
	if overrideErr != nil || overrideBytes > baseBytes {
		return override
	}
	return base
}

func gpuRequirementRank(value json.RawMessage) int {
	value = json.RawMessage(strings.TrimSpace(string(value)))
	switch string(value) {
	case "", "null", "false":
		return 0
	case `"optional"`:
		return 1
	default:
		return 2
	}
}

func mergeMap(base, override map[string]string) map[string]string {
	result := maps.Clone(base)
	if result == nil && len(override) > 0 {
		result = map[string]string{}
	}
	for key, value := range override {
		result[key] = value
	}
	return result
}

func mergeRemoteEnvironment(base, override RemoteEnvironment) RemoteEnvironment {
	result := maps.Clone(base)
	if result == nil && len(override) > 0 {
		result = RemoteEnvironment{}
	}
	for key, value := range override {
		result[key] = value
	}
	return result
}

func mergeRawMap(base, override map[string]json.RawMessage) map[string]json.RawMessage {
	result := maps.Clone(base)
	if result == nil && len(override) > 0 {
		result = map[string]json.RawMessage{}
	}
	for key, value := range override {
		result[key] = slices.Clone(value)
	}
	return result
}

func mergeStructMap[T any](base, override map[string]T) map[string]T {
	result := maps.Clone(base)
	if result == nil && len(override) > 0 {
		result = map[string]T{}
	}
	for key, value := range override {
		result[key] = value
	}
	return result
}

func appendUnique(base, override []string) []string {
	result := slices.Clone(base)
	for _, value := range override {
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func appendUniqueComparable[T comparable](base, override []T) []T {
	result := slices.Clone(base)
	for _, value := range override {
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func mergeMounts(base, override []Mount) []Mount {
	result := slices.Clone(base)
	for _, item := range override {
		result = slices.DeleteFunc(result, func(existing Mount) bool { return existing.Target == item.Target })
		result = append(result, item)
	}
	return result
}

func mergeCustomizations(base, override map[string]json.RawMessage) map[string]json.RawMessage {
	result := maps.Clone(base)
	if result == nil && len(override) > 0 {
		result = map[string]json.RawMessage{}
	}
	for key, raw := range override {
		var left, right any
		leftDecoder := json.NewDecoder(bytes.NewReader(result[key]))
		leftDecoder.UseNumber()
		rightDecoder := json.NewDecoder(bytes.NewReader(raw))
		rightDecoder.UseNumber()
		if leftDecoder.Decode(&left) == nil && rightDecoder.Decode(&right) == nil {
			if merged, ok := mergeJSON(left, right).(map[string]any); ok {
				if encoded, err := json.Marshal(merged); err == nil {
					result[key] = encoded
					continue
				}
			}
		}
		result[key] = slices.Clone(raw)
	}
	return result
}

func mergeJSON(base, override any) any {
	left, leftOK := base.(map[string]any)
	right, rightOK := override.(map[string]any)
	if leftOK && rightOK {
		result := maps.Clone(left)
		for key, value := range right {
			result[key] = mergeJSON(result[key], value)
		}
		return result
	}
	leftArray, leftOK := base.([]any)
	rightArray, rightOK := override.([]any)
	if leftOK && rightOK {
		result := slices.Clone(leftArray)
		for _, value := range rightArray {
			if !slices.ContainsFunc(result, func(item any) bool { return jsonEqual(item, value) }) {
				result = append(result, value)
			}
		}
		return result
	}
	return override
}

func jsonEqual(left, right any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}

func mergeCommand(base, override Command, name string) Command {
	if len(base.Value) == 0 {
		return override
	}
	if len(override.Value) == 0 {
		return base
	}
	entries := map[string]json.RawMessage{}
	add := func(prefix string, command Command) {
		var object map[string]json.RawMessage
		if json.Unmarshal(command.Value, &object) == nil && object != nil {
			for key, value := range object {
				entries[prefix+"-"+key] = value
			}
			return
		}
		entries[prefix] = command.Value
	}
	add("base", base)
	add(name, override)
	encoded, _ := json.Marshal(entries)
	return Command{Value: encoded}
}
