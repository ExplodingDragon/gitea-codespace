// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"gitea.dev/codespace/devcontainer"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

const (
	proxyUsernameKey = "proxy-username"
	proxyPasswordKey = "proxy-password"
	proxyCAKey       = "proxy-ca.crt"
)

func templateProxySecretName(templateName string) string { return templateName + "-proxy" }

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

// reconcileProxySecretData replaces derived proxy values while preserving
// unrelated Secret data and, unless requested otherwise, existing credentials.
func reconcileProxySecretData(data map[string][]byte, proxy *api.OutboundProxy, username, password string, clearCredential bool) (map[string][]byte, error) {
	if len(username) > 512 || len(password) > 4096 {
		return nil, fmt.Errorf("proxy credential is too large")
	}
	if data == nil {
		data = make(map[string][]byte)
	}
	for _, name := range devcontainer.ProxyVariableNames() {
		delete(data, name)
	}
	delete(data, proxyCAKey)
	if clearCredential || proxy == nil {
		delete(data, proxyUsernameKey)
		delete(data, proxyPasswordKey)
	}
	if proxy == nil {
		return data, nil
	}
	if username != "" || password != "" {
		if username == "" || password == "" {
			return nil, fmt.Errorf("proxy username and password must be configured together")
		}
		data[proxyUsernameKey], data[proxyPasswordKey] = []byte(username), []byte(password)
	}
	username, password = string(data[proxyUsernameKey]), string(data[proxyPasswordKey])
	if (username == "") != (password == "") {
		return nil, fmt.Errorf("stored proxy credential is incomplete")
	}
	environment, err := outboundProxyEnvironment(proxy, username, password)
	if err != nil {
		return nil, err
	}
	for name, value := range environment {
		data[name] = []byte(value)
	}
	if proxy.CABundle != "" {
		data[proxyCAKey] = []byte(proxy.CABundle)
	}
	return data, nil
}

func outboundProxyEnvironment(proxy *api.OutboundProxy, username, password string) (map[string]string, error) {
	if proxy == nil {
		return nil, nil
	}
	withCredential := func(value string) (string, error) {
		if value == "" || username == "" {
			return value, nil
		}
		parsed, err := url.Parse(value)
		if err != nil {
			return "", err
		}
		parsed.User = url.UserPassword(username, password)
		return parsed.String(), nil
	}
	httpProxy, err := withCredential(proxy.HTTPProxy)
	if err != nil {
		return nil, err
	}
	httpsProxy, err := withCredential(proxy.HTTPSProxy)
	if err != nil {
		return nil, err
	}
	socksProxy, err := withCredential(proxy.SOCKSProxy)
	if err != nil {
		return nil, err
	}
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
	return environment, nil
}
