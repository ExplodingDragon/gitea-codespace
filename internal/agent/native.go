// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"

	"gitea.dev/codespace/internal/devcontainerruntime"
	"gitea.dev/codespace/internal/runtimeplatform"
	"tags.cncf.io/container-device-interface/pkg/cdi"
	"tags.cncf.io/container-device-interface/pkg/parser"
	cditypes "tags.cncf.io/container-device-interface/specs-go"
)

const nativeCDISpecDirectory = runtimeRunDirectory + "/cdi"

const (
	nativeEnvironmentCDIKind   = "codespace.gitea.dev/environment"
	nativeEnvironmentCDIDevice = "current"
)

// NativeDevices contains the allocated device access prepared for the primary
// Dev Container and the filtered CDI directory used by its Docker daemon.
type NativeDevices struct {
	DevContainer     devcontainerruntime.NativeContainerConfiguration
	CDISpecDirectory string
}

type allocatedCDIDevice struct {
	physical string
	alias    string
}

// PrepareNativeDevices validates Device Plugin output and writes a filtered
// CDI view for the nested Docker daemon. Only devices allocated to this Pod
// are copied into the writable runtime directory.
func PrepareNativeDevices(configuration *runtimeplatform.NativeConfiguration) (*NativeDevices, error) {
	if configuration == nil {
		return nil, nil
	}
	prepared := &NativeDevices{}
	forwardedEnvironment := map[string]string{}
	allocatedDevices := []allocatedCDIDevice{}
	for _, capability := range configuration.DevContainerCapAdd {
		prepared.DevContainer.Capabilities = append(prepared.DevContainer.Capabilities, string(capability))
	}
	requestedCDI := map[string]struct{}{}
	for pluginIndex, plugin := range configuration.DevicePlugins {
		prepared.DevContainer.ProvidesGPU = prepared.DevContainer.ProvidesGPU || plugin.ProvidesGPU
		prepared.DevContainer.Devices = append(prepared.DevContainer.Devices, plugin.HostAccess.Devices...)
		prepared.DevContainer.Mounts = append(prepared.DevContainer.Mounts, plugin.HostAccess.Mounts...)
		for _, name := range plugin.HostAccess.ForwardEnvironment {
			value, exists := os.LookupEnv(name)
			if !exists {
				return nil, fmt.Errorf("device resource %s did not provide environment %s", plugin.ResourceName, name)
			}
			forwardedEnvironment[name] = value
		}
		if plugin.CDI == nil {
			continue
		}
		value := strings.TrimSpace(os.Getenv(plugin.CDI.DeviceIDEnvironment))
		if value == "" {
			return nil, fmt.Errorf("device resource %s did not provide CDI device IDs in %s", plugin.ResourceName, plugin.CDI.DeviceIDEnvironment)
		}
		ids := strings.Split(value, ",")
		if int64(len(ids)) != plugin.Count {
			return nil, fmt.Errorf("device resource %s allocated %d CDI device IDs, expected %d", plugin.ResourceName, len(ids), plugin.Count)
		}
		vendor, class := parser.ParseQualifier(plugin.CDI.Kind)
		for deviceIndex, value := range ids {
			id := strings.TrimSpace(value)
			if strings.EqualFold(id, "all") || strings.EqualFold(id, "none") || strings.EqualFold(id, "void") || parser.ValidateDeviceName(id) != nil {
				return nil, fmt.Errorf("device resource %s provided invalid CDI device ID %q", plugin.ResourceName, id)
			}
			qualified := parser.QualifiedName(vendor, class, id)
			if _, duplicate := requestedCDI[qualified]; duplicate {
				return nil, fmt.Errorf("CDI device %s was allocated more than once", qualified)
			}
			requestedCDI[qualified] = struct{}{}
			alias := parser.QualifiedName(vendor, class, fmt.Sprintf("codespace-%d-%d", pluginIndex, deviceIndex))
			allocatedDevices = append(allocatedDevices, allocatedCDIDevice{physical: qualified, alias: alias})
			prepared.DevContainer.CDIDeviceIDs = append(prepared.DevContainer.CDIDeviceIDs, alias)
		}
	}
	if len(allocatedDevices) == 0 && len(forwardedEnvironment) == 0 {
		return prepared, nil
	}
	if len(forwardedEnvironment) > 0 {
		prepared.DevContainer.CDIDeviceIDs = append(prepared.DevContainer.CDIDeviceIDs, parser.QualifiedName("codespace.gitea.dev", "environment", nativeEnvironmentCDIDevice))
	}
	resolvedEnvironment, err := writeNativeCDISpecs(allocatedDevices, forwardedEnvironment, cdi.DefaultSpecDirs, nativeCDISpecDirectory)
	if err != nil {
		return nil, err
	}
	prepared.DevContainer.Environment = resolvedEnvironment
	prepared.CDISpecDirectory = nativeCDISpecDirectory
	return prepared, nil
}

func writeNativeCDISpecs(devices []allocatedCDIDevice, environment map[string]string, sourceDirectories []string, outputDirectory string) (map[string]string, error) {
	source, err := cdi.NewCache(cdi.WithSpecDirs(sourceDirectories...), cdi.WithAutoRefresh(false))
	if err != nil {
		return nil, fmt.Errorf("open node CDI specifications: %w", err)
	}
	// A malformed unrelated node spec must not hide a valid allocated device;
	// each requested device is checked explicitly below.
	_ = source.Refresh()
	type selectedSpec struct {
		raw     *cditypes.Spec
		devices map[string]struct{}
	}
	selected := map[string]*selectedSpec{}
	aliases := make(map[string]string, len(devices))
	resolvedEnvironment := map[string]string{}
	appliedSpecEnvironment := map[string]struct{}{}
	for _, allocated := range devices {
		device := source.GetDevice(allocated.physical)
		if device == nil {
			return nil, fmt.Errorf("allocated CDI device %s is not present in the node specifications", allocated.physical)
		}
		spec := device.GetSpec()
		if _, applied := appliedSpecEnvironment[spec.GetPath()]; !applied {
			mergeCDIEnvironment(resolvedEnvironment, spec.ContainerEdits.Env)
			appliedSpecEnvironment[spec.GetPath()] = struct{}{}
		}
		mergeCDIEnvironment(resolvedEnvironment, device.ContainerEdits.Env)
		entry := selected[spec.GetPath()]
		if entry == nil {
			encoded, err := json.Marshal(spec.Spec)
			if err != nil {
				return nil, fmt.Errorf("copy CDI specification %s: %w", spec.GetPath(), err)
			}
			var raw cditypes.Spec
			if err := json.Unmarshal(encoded, &raw); err != nil {
				return nil, fmt.Errorf("copy CDI specification %s: %w", spec.GetPath(), err)
			}
			entry = &selectedSpec{raw: &raw, devices: map[string]struct{}{}}
			selected[spec.GetPath()] = entry
		}
		entry.devices[device.Name] = struct{}{}
		_, _, aliasName, err := parser.ParseQualifiedName(allocated.alias)
		if err != nil {
			return nil, fmt.Errorf("parse internal CDI device alias %s: %w", allocated.alias, err)
		}
		aliases[allocated.physical] = aliasName
	}
	maps.Copy(resolvedEnvironment, environment)
	if err := os.RemoveAll(outputDirectory); err != nil {
		return nil, fmt.Errorf("reset filtered CDI directory: %w", err)
	}
	if err := os.MkdirAll(outputDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create filtered CDI directory: %w", err)
	}
	destination, err := cdi.NewCache(cdi.WithSpecDirs(outputDirectory), cdi.WithAutoRefresh(false))
	if err != nil {
		return nil, fmt.Errorf("open filtered CDI directory: %w", err)
	}
	paths := make([]string, 0, len(selected))
	for path := range selected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for index, path := range paths {
		entry := selected[path]
		devices := entry.raw.Devices[:0]
		vendor, class := parser.ParseQualifier(entry.raw.Kind)
		for _, device := range entry.raw.Devices {
			if _, keep := entry.devices[device.Name]; keep {
				physical := parser.QualifiedName(vendor, class, device.Name)
				device.Name = aliases[physical]
				devices = append(devices, device)
			}
		}
		entry.raw.Devices = devices
		if err := destination.WriteSpec(entry.raw, fmt.Sprintf("codespace-%d.json", index)); err != nil {
			return nil, fmt.Errorf("write filtered CDI specification: %w", err)
		}
	}
	if len(environment) > 0 {
		names := make([]string, 0, len(environment))
		for name := range environment {
			names = append(names, name)
		}
		sort.Strings(names)
		values := make([]string, 0, len(names))
		for _, name := range names {
			values = append(values, name+"="+environment[name])
		}
		if err := destination.WriteSpec(&cditypes.Spec{
			Version: cditypes.CurrentVersion,
			Kind:    nativeEnvironmentCDIKind,
			Devices: []cditypes.Device{{Name: nativeEnvironmentCDIDevice, ContainerEdits: cditypes.ContainerEdits{Env: values}}},
		}, "codespace-environment.json"); err != nil {
			return nil, fmt.Errorf("write forwarded device environment CDI specification: %w", err)
		}
	}
	return resolvedEnvironment, nil
}

func mergeCDIEnvironment(environment map[string]string, assignments []string) {
	for _, assignment := range assignments {
		name, value, _ := strings.Cut(assignment, "=")
		environment[name] = value
	}
}
