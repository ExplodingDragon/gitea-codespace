// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"slices"
	"strings"

	"gitea.dev/codespace/devcontainer"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

const proxyCAKey = "proxy-ca.crt"

func runtimeProxySecretName(codespaceName string) string { return codespaceName + "-proxy" }

func proxyEnvironmentFromSecret(secretName string) []corev1.EnvVar {
	names := devcontainer.ProxyVariableNames()
	result := make([]corev1.EnvVar, 0, len(names))
	for _, name := range names {
		result = append(result, corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: name, Optional: ptr.To(true),
		}}})
	}
	return result
}

// updateProxySecretData replaces derived proxy values while preserving
// unrelated Secret data.
func updateProxySecretData(data map[string][]byte, proxy *api.OutboundProxy) map[string][]byte {
	if data == nil {
		data = make(map[string][]byte)
	}
	for _, name := range devcontainer.ProxyVariableNames() {
		delete(data, name)
	}
	delete(data, proxyCAKey)
	if proxy == nil {
		return data
	}
	for name, value := range outboundProxyEnvironment(proxy) {
		data[name] = []byte(value)
	}
	if proxy.CABundle != "" {
		data[proxyCAKey] = []byte(proxy.CABundle)
	}
	return data
}

func outboundProxyEnvironment(proxy *api.OutboundProxy) map[string]string {
	if proxy == nil {
		return nil
	}
	httpProxy, httpsProxy, socksProxy := proxy.HTTPProxy, proxy.HTTPSProxy, proxy.SOCKSProxy
	if httpProxy == "" {
		httpProxy = socksProxy
	}
	if httpsProxy == "" {
		httpsProxy = socksProxy
	}
	bypass := slices.Clone(proxy.NoProxy)
	bypass = append(bypass, "localhost", "127.0.0.1", "::1", ".svc", ".svc.cluster.local")
	seen := make(map[string]struct{}, len(bypass))
	values := bypass[:0]
	for _, value := range bypass {
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, value)
	}
	environment := make(map[string]string, len(devcontainer.ProxyVariableNames()))
	for _, pair := range [][2]string{{"HTTP_PROXY", httpProxy}, {"HTTPS_PROXY", httpsProxy}, {"ALL_PROXY", socksProxy}, {"NO_PROXY", strings.Join(values, ",")}} {
		if pair[1] == "" {
			continue
		}
		environment[pair[0]], environment[strings.ToLower(pair[0])] = pair[1], pair[1]
	}
	return environment
}
