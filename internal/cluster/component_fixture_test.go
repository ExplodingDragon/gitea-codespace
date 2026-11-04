// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testComponentResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
}

func testGatewayComponent(name string, uid types.UID) *api.CodespaceComponent {
	return &api.CodespaceComponent{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid, Generation: 1},
		Spec: api.CodespaceComponentSpec{
			Role: "gateway", DisplayName: "Primary gateway", Resources: testComponentResources(),
			Gateway: &api.GatewayComponentSpec{
				Replicas:        2,
				EndpointBaseURL: "https://gateway.example.com", SSHExternalAddress: "gateway.example.com:22",
				HTTPExposure: api.GatewayHTTPExposure{Mode: "external"}, SSHExposure: api.GatewaySSHExposure{Mode: "external"},
				SSH: api.GatewaySSHSettings{HandshakeTimeout: "30s", MaxChannelsPerConnection: 32, Auth: api.GatewaySSHAuth{
					MaxAttemptsPerIP: 30, MaxAttemptsPerCodespace: 20, MaxAttemptsPerIPCodespace: 10, MaxAttemptsPerPublicKey: 30, FailureWindow: "10m",
				}},
				Sessions: api.GatewaySessions{TTL: "8h", IdleTimeout: "30m", RevalidateInterval: "5m", MaxPerCodespace: 32, MaxPerUser: 128},
				Limits:   api.GatewayLimits{MaxInflightTotal: 4096, MaxInflightPerSession: 32, PublicMaxConnectionsPerEndpoint: 64, PublicMaxConnectionsPerIP: 16, ValidationMaxInflight: 128},
			},
		},
	}
}

func testCacheComponent(name string, uid types.UID) *api.CodespaceComponent {
	return &api.CodespaceComponent{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid, Generation: 1},
		Spec: api.CodespaceComponentSpec{
			Role: "cache", DisplayName: "Build cache", Resources: testComponentResources(),
			Cache: &api.CacheComponentSpec{
				Enabled: true, MaxSize: "768MiB", MaxAge: "24h", GCInterval: "1h",
				Storage:   api.CacheStorage{Driver: "filesystem", VolumeSize: "1Gi", MinFreeSpace: "128MiB"},
				Upstreams: map[string]api.CacheUpstream{"docker.io": {Allow: []string{"library/*"}}},
			},
		},
	}
}

func testComponentCredential(component *api.CodespaceComponent, data map[string][]byte) *corev1.Secret {
	name := component.Name + "-cache"
	if component.Spec.Role == "gateway" {
		name = component.Name + "-ssh-host-key"
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "codespace-system", UID: types.UID(name + "-uid"), Labels: map[string]string{ComponentUIDLabel: string(component.UID)},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(component, api.GroupVersion.WithKind("CodespaceComponent"))},
		},
		Data: data,
	}
}
