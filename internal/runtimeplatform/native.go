// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package runtimeplatform defines configuration shared by the Manager and the
// Runtime Pod Agent for host-native environments.
package runtimeplatform

import corev1 "k8s.io/api/core/v1"

// NativeConfiguration configures the outer Runtime Pod and the primary Dev
// Container when Kubernetes runs them on the node kernel.
type NativeConfiguration struct {
	RuntimeCapAdd      []corev1.Capability   `json:"runtimeCapAdd,omitempty"`
	DevicePlugins      []DevicePluginRequest `json:"devicePlugins,omitempty"`
	DevContainerCapAdd []corev1.Capability   `json:"devContainerCapAdd,omitempty"`
}

// DevicePluginRequest reserves one Kubernetes extended resource. CDI and
// HostAccess describe how the allocated devices reach the nested Dev Container.
type DevicePluginRequest struct {
	ResourceName corev1.ResourceName   `json:"resourceName"`
	Count        int64                 `json:"count"`
	ProvidesGPU  bool                  `json:"providesGPU,omitempty"`
	CDI          *CDIDevicePropagation `json:"cdi,omitempty"`
	HostAccess   HostAccess            `json:"hostAccess,omitempty"`
}

// CDIDevicePropagation converts Device Plugin allocation IDs into CDI device
// names understood by the nested Docker daemon.
type CDIDevicePropagation struct {
	Kind                string `json:"kind"`
	DeviceIDEnvironment string `json:"deviceIDEnvironment"`
}

// HostAccess grants the Runtime Pod explicit access to node paths and forwards
// Device Plugin environment variables to the primary Dev Container.
type HostAccess struct {
	Devices            []DeviceMapping `json:"devices,omitempty"`
	Mounts             []MountMapping  `json:"mounts,omitempty"`
	ForwardEnvironment []string        `json:"forwardEnvironment,omitempty"`
}

// DeviceMapping exposes a node device to the primary Dev Container.
type DeviceMapping struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Permissions string `json:"permissions"`
}

// MountMapping exposes a node path to the primary Dev Container.
type MountMapping struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// DeepCopyInto supports Kubernetes API deep-copy generation for the enclosing
// EnvironmentTemplate types.
func (in *NativeConfiguration) DeepCopyInto(out *NativeConfiguration) {
	*out = *in
	out.RuntimeCapAdd = append([]corev1.Capability(nil), in.RuntimeCapAdd...)
	out.DevContainerCapAdd = append([]corev1.Capability(nil), in.DevContainerCapAdd...)
	if in.DevicePlugins == nil {
		return
	}
	out.DevicePlugins = make([]DevicePluginRequest, len(in.DevicePlugins))
	for index := range in.DevicePlugins {
		out.DevicePlugins[index] = in.DevicePlugins[index]
		if in.DevicePlugins[index].CDI != nil {
			value := *in.DevicePlugins[index].CDI
			out.DevicePlugins[index].CDI = &value
		}
		out.DevicePlugins[index].HostAccess.Devices = append([]DeviceMapping(nil), in.DevicePlugins[index].HostAccess.Devices...)
		out.DevicePlugins[index].HostAccess.Mounts = append([]MountMapping(nil), in.DevicePlugins[index].HostAccess.Mounts...)
		out.DevicePlugins[index].HostAccess.ForwardEnvironment = append([]string(nil), in.DevicePlugins[index].HostAccess.ForwardEnvironment...)
	}
}

func (in *NativeConfiguration) DeepCopy() *NativeConfiguration {
	if in == nil {
		return nil
	}
	out := new(NativeConfiguration)
	in.DeepCopyInto(out)
	return out
}
