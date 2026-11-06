// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEnvironmentTemplateRejectsInvalidName(t *testing.T) {
	for _, name := range []string{"", "Standard", "standard_gpu", "standard.gpu", strings.Repeat("a", 64)} {
		template := EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: name}}
		require.ErrorContains(t, template.Validate(), "lowercase DNS label", name)
	}
}

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

func TestOutboundProxyValidation(t *testing.T) {
	valid := &OutboundProxy{
		HTTPProxy:  "http://user:p%40ssword@proxy.example:3128",
		HTTPSProxy: "https://proxy.example:8443",
		SOCKSProxy: "socks5h://proxy.example:1080",
		NoProxy:    []string{"registry.internal", "10.0.0.0/8"},
	}
	require.NoError(t, validateOutboundProxy(valid))

	for _, proxy := range []*OutboundProxy{
		{},
		{HTTPProxy: "http://proxy.example:3128/path"},
		{HTTPSProxy: "https://proxy.example"},
		{HTTPSProxy: "https://proxy.example:65536"},
		{SOCKSProxy: "http://proxy.example:1080"},
		{HTTPProxy: "http://proxy.example:3128", NoProxy: []string{"example", "EXAMPLE"}},
	} {
		require.Error(t, validateOutboundProxy(proxy))
	}
}
