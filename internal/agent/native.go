// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gitea.dev/codespace/internal/devcontainerruntime"
	"gitea.dev/codespace/internal/runtimeplatform"
	"golang.org/x/sys/unix"
	"tags.cncf.io/container-device-interface/pkg/cdi"
	"tags.cncf.io/container-device-interface/pkg/parser"
	cditypes "tags.cncf.io/container-device-interface/specs-go"
)

const (
	nativeCDISpecDirectory = runtimeRunDirectory + "/cdi"
	nativeCDIKind          = "codespace.gitea.dev/native"
	nativeCDIDevice        = nativeCDIKind + "=current"
)

// NativeDevices contains the current allocation and its Runtime-local CDI view.
type NativeDevices struct {
	DevContainer     devcontainerruntime.NativeContainerConfiguration
	CDISpecDirectory string
}

func prepareNativeDevices(configuration *runtimeplatform.NativeConfiguration, sourceDirectories []string, outputDirectory string) (*NativeDevices, error) {
	if configuration == nil {
		return nil, nil
	}
	prepared := &NativeDevices{}
	for _, capability := range configuration.DevContainerCapAdd {
		prepared.DevContainer.Capabilities = append(prepared.DevContainer.Capabilities, string(capability))
	}
	if len(configuration.DevicePlugins) == 0 && len(configuration.AdditionalGroups) == 0 {
		return prepared, nil
	}
	resolved, err := resolveNativeCDIEdits(configuration.DevicePlugins, sourceDirectories)
	if err != nil {
		return nil, err
	}
	prepared.DevContainer.Environment = map[string]string{}
	for _, value := range resolved.Env {
		name, value, _ := strings.Cut(value, "=")
		prepared.DevContainer.Environment[name] = value
	}
	manualMounts := map[runtimeplatform.MountMapping]bool{}
	for pluginIndex, plugin := range configuration.DevicePlugins {
		prepared.DevContainer.ProvidesGPU = prepared.DevContainer.ProvidesGPU || plugin.ProvidesGPU
		for _, name := range plugin.HostAccess.ForwardEnvironment {
			value, exists := os.LookupEnv(name)
			if !exists {
				return nil, fmt.Errorf("device resource %s did not provide environment %s", plugin.ResourceName, name)
			}
			prepared.DevContainer.Environment[name] = value
		}
		for _, mapping := range plugin.HostAccess.Devices {
			count := len(resolved.DeviceNodes)
			err := filepath.WalkDir(mapping.Source, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := os.Stat(path)
				if err != nil {
					return err
				}
				if info.IsDir() {
					if entry.Type()&os.ModeSymlink != 0 {
						return fmt.Errorf("device directory %s must not be a symbolic link", path)
					}
					return nil
				}
				if info.Mode()&os.ModeDevice == 0 {
					if path == mapping.Source {
						return fmt.Errorf("%s is not a device or device directory", path)
					}
					return nil
				}
				relative, err := filepath.Rel(mapping.Source, path)
				if err != nil {
					return err
				}
				node := &cditypes.DeviceNode{Path: filepath.Join(mapping.Target, relative), Permissions: mapping.Permissions}
				if err := resolveNativeDeviceNode(node, path); err != nil {
					return err
				}
				resolved.DeviceNodes = append(resolved.DeviceNodes, node)
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("device resource %s mapping %s: %w", plugin.ResourceName, mapping.Source, err)
			}
			if len(resolved.DeviceNodes) == count {
				return nil, fmt.Errorf("device resource %s mapping %s contains no devices", plugin.ResourceName, mapping.Source)
			}
		}
		for mountIndex, mapping := range plugin.HostAccess.Mounts {
			if manualMounts[mapping] {
				continue
			}
			manualMounts[mapping] = true
			source := mapping.Source
			if mapping.SourceScope == "node" {
				source = runtimeplatform.NativeMountPath(pluginIndex, mountIndex)
			}
			if _, err := os.Stat(source); err != nil {
				return nil, fmt.Errorf("device resource %s mount %s: %w", plugin.ResourceName, mapping.Source, err)
			}
			options := []string{"bind"}
			if mapping.ReadOnly {
				options = append(options, "ro")
			}
			resolved.Mounts = append(resolved.Mounts, &cditypes.Mount{HostPath: source, ContainerPath: mapping.Target, Options: options})
		}
	}
	// One entry keeps saved Docker requests independent of physical device names
	// and of the number of devices backing a shared allocation.
	resolved.AdditionalGIDs = append(resolved.AdditionalGIDs, configuration.AdditionalGroups...)
	slices.Sort(resolved.AdditionalGIDs)
	resolved.AdditionalGIDs = slices.Compact(resolved.AdditionalGIDs)
	prepared.DevContainer.AdditionalGroups = resolved.AdditionalGIDs
	resolved.Env = nil
	for name, value := range prepared.DevContainer.Environment {
		resolved.Env = append(resolved.Env, name+"="+value)
	}
	slices.Sort(resolved.Env)
	targets := map[string]any{}
	nodes := resolved.DeviceNodes[:0]
	for _, node := range resolved.DeviceNodes {
		if previous, exists := targets[node.Path]; exists {
			if !reflect.DeepEqual(previous, node) {
				return nil, fmt.Errorf("conflicting native device target %s", node.Path)
			}
			continue
		}
		targets[node.Path] = node
		nodes = append(nodes, node)
	}
	resolved.DeviceNodes = nodes
	mounts := resolved.Mounts[:0]
	for _, mount := range resolved.Mounts {
		if previous, exists := targets[mount.ContainerPath]; exists {
			if !reflect.DeepEqual(previous, mount) {
				return nil, fmt.Errorf("conflicting native mount target %s", mount.ContainerPath)
			}
			continue
		}
		targets[mount.ContainerPath] = mount
		mounts = append(mounts, mount)
	}
	resolved.Mounts = mounts
	if err := (&cdi.ContainerEdits{ContainerEdits: resolved}).Validate(); err != nil {
		return nil, fmt.Errorf("validate native device configuration: %w", err)
	}
	if err := os.MkdirAll(outputDirectory, 0o700); err != nil {
		return nil, err
	}
	destination, err := cdi.NewCache(cdi.WithSpecDirs(outputDirectory), cdi.WithAutoRefresh(false))
	if err != nil {
		return nil, err
	}
	spec := &cditypes.Spec{Kind: nativeCDIKind,
		Devices: []cditypes.Device{{Name: "current", ContainerEdits: *resolved}},
	}
	// Declare the version required by the edits so Docker can read the spec
	// independently of the CDI library version used by the Agent.
	spec.Version, err = cditypes.MinimumRequiredVersion(spec)
	if err != nil {
		return nil, fmt.Errorf("determine native device configuration version: %w", err)
	}
	if err := destination.WriteSpec(spec, "native.json"); err != nil {
		return nil, fmt.Errorf("write native device configuration: %w", err)
	}
	prepared.DevContainer.CDIDeviceIDs = []string{nativeCDIDevice}
	prepared.CDISpecDirectory = outputDirectory
	return prepared, nil
}

func resolveNativeCDIEdits(plugins []runtimeplatform.DevicePluginRequest, sourceDirectories []string) (*cditypes.ContainerEdits, error) {
	edits := &cdi.ContainerEdits{ContainerEdits: &cditypes.ContainerEdits{}}
	var source *cdi.Cache
	appliedSpecs, appliedDevices := map[string]bool{}, map[string]bool{}
	for _, plugin := range plugins {
		if plugin.CDI == nil {
			continue
		}
		if source == nil {
			var err error
			source, err = cdi.NewCache(cdi.WithSpecDirs(sourceDirectories...), cdi.WithAutoRefresh(false))
			if err != nil {
				return nil, fmt.Errorf("open node CDI specifications: %w", err)
			}
		}
		names, err := selectNativeCDIDevices(source, plugin.CDI)
		if err != nil {
			return nil, fmt.Errorf("device resource %s: %w", plugin.ResourceName, err)
		}
		for _, name := range names {
			if appliedDevices[name] {
				continue
			}
			device := source.GetDevice(name)
			if device == nil {
				return nil, fmt.Errorf("allocated CDI device %s is not present in the node specifications", name)
			}
			spec := device.GetSpec()
			if !appliedSpecs[spec.GetPath()] {
				edits.Append(&cdi.ContainerEdits{ContainerEdits: &spec.ContainerEdits})
				appliedSpecs[spec.GetPath()] = true
			}
			edits.Append(&cdi.ContainerEdits{ContainerEdits: &device.ContainerEdits})
			appliedDevices[name] = true
		}
	}
	// Append owns the slices, but their nodes and mounts still belong to the cache.
	for index, original := range edits.DeviceNodes {
		node := *original
		// The outer CRI has already placed this node at its container path.
		if err := resolveNativeDeviceNode(&node, node.Path); err != nil {
			return nil, err
		}
		edits.DeviceNodes[index] = &node
	}
	for index, original := range edits.Mounts {
		mount := *original
		if mount.Type != "bind" && !slices.Contains(mount.Options, "bind") && !slices.Contains(mount.Options, "rbind") {
			continue
		}
		if _, err := os.Stat(mount.ContainerPath); err != nil {
			return nil, fmt.Errorf("CDI mount %s is unavailable in Runtime: %w", mount.ContainerPath, err)
		}
		mount.HostPath = mount.ContainerPath
		edits.Mounts[index] = &mount
	}
	for _, hook := range edits.Hooks {
		info, err := os.Stat(hook.Path)
		if err != nil {
			return nil, fmt.Errorf("CDI hook %s is unavailable in Runtime: %w", hook.Path, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return nil, fmt.Errorf("CDI hook %s must be executable in Runtime", hook.Path)
		}
	}
	return edits.ContainerEdits, nil
}

func selectNativeCDIDevices(source *cdi.Cache, configuration *runtimeplatform.CDIDevicePropagation) ([]string, error) {
	var names []string
	switch configuration.Selection {
	case "environment":
		for _, value := range strings.Split(os.Getenv(configuration.DeviceIDEnvironment), ",") {
			name := strings.TrimSpace(value)
			if !strings.Contains(name, "=") {
				name = configuration.Kind + "=" + name
			}
			vendor, class, id, err := parser.ParseQualifiedName(name)
			if err != nil || vendor+"/"+class != configuration.Kind || slices.Contains([]string{"all", "none", "void"}, strings.ToLower(id)) {
				return nil, fmt.Errorf("invalid CDI device %q in %s", value, configuration.DeviceIDEnvironment)
			}
			names = append(names, name)
		}
	case "deviceNodes":
		visible := map[string]bool{}
		for _, pattern := range configuration.DeviceNodePaths {
			paths, err := filepath.Glob(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid CDI device path pattern %s: %w", pattern, err)
			}
			for _, path := range paths {
				info, err := os.Stat(path)
				if err != nil {
					return nil, err
				}
				if info.Mode()&os.ModeDevice != 0 {
					visible[path] = true
				}
			}
		}
		owners := map[string]string{}
		for _, name := range source.ListDevices() {
			device := source.GetDevice(name)
			if device.GetSpec().Kind != configuration.Kind || slices.Contains([]string{"all", "none", "void"}, strings.ToLower(device.Name)) {
				continue
			}
			var anchors []string
			matches := true
			for _, node := range device.ContainerEdits.DeviceNodes {
				for _, pattern := range configuration.DeviceNodePaths {
					if matched, _ := filepath.Match(pattern, node.Path); !matched {
						continue
					}
					if !visible[node.Path] {
						matches = false
						break
					}
					anchors = append(anchors, node.Path)
				}
			}
			if !matches || len(anchors) == 0 {
				continue
			}
			for _, path := range anchors {
				if previous, exists := owners[path]; exists && previous != name {
					return nil, fmt.Errorf("CDI device node %s ambiguously matches %s and %s; use allocation environment or manual mappings", path, previous, name)
				}
				owners[path] = name
			}
			names = append(names, name)
		}
		for path := range visible {
			if owners[path] == "" {
				return nil, fmt.Errorf("Runtime device %s has no matching %s CDI device", path, configuration.Kind)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported CDI selection %q", configuration.Selection)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no allocated CDI devices found for %s", configuration.Kind)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func resolveNativeDeviceNode(node *cditypes.DeviceNode, source string) error {
	var stat unix.Stat_t
	if err := unix.Stat(source, &stat); err != nil {
		return fmt.Errorf("device %s is unavailable in Runtime: %w", source, err)
	}
	kind := "c"
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFCHR:
	case unix.S_IFBLK:
		kind = "b"
	default:
		return fmt.Errorf("Runtime path %s is not a character or block device", source)
	}
	major, minor := int64(unix.Major(stat.Rdev)), int64(unix.Minor(stat.Rdev))
	if node.Type != "" && node.Type != kind && !(node.Type == "u" && kind == "c") || node.Major != 0 && (node.Major != major || node.Minor != minor) {
		return fmt.Errorf("Runtime device %s does not match CDI type or device number", source)
	}
	node.HostPath, node.Type, node.Major, node.Minor = source, kind, major, minor
	if node.Permissions == "" {
		node.Permissions = "rwm"
	}
	if node.FileMode == nil {
		mode := os.FileMode(stat.Mode & 0o777)
		node.FileMode = &mode
	}
	if node.UID == nil {
		node.UID = &stat.Uid
	}
	if node.GID == nil {
		node.GID = &stat.Gid
	}
	return nil
}
