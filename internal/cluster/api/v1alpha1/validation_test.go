// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestEnvironmentConfigurationRejectsUnsupportedResourceLimit(t *testing.T) {
	configuration := EnvironmentConfiguration{
		Isolation:        IsolationSysbox,
		RuntimeClassName: "sysbox-runc",
		StorageClassName: "local-path",
		Storage:          corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceName("example.com/device"): resource.MustParse("1")},
		},
		AccessMode:    corev1.ReadWriteOnce,
		GitSSHKeyType: "ed25519",
	}

	require.ErrorContains(t, configuration.Validate(), "only positive CPU and memory limits")
}
