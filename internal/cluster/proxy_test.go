// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestReconcileProxySecretData(t *testing.T) {
	proxy := &api.OutboundProxy{HTTPProxy: "http://proxy.example:3128", SOCKSProxy: "socks5h://proxy.example:1080", NoProxy: []string{"registry.internal"}, CABundle: "ca"}
	data, err := reconcileProxySecretData(map[string][]byte{"unrelated": []byte("value")}, proxy, "user", "password", false)
	require.NoError(t, err)
	require.Equal(t, "http://user:password@proxy.example:3128", string(data["HTTP_PROXY"]))
	require.Equal(t, "socks5h://user:password@proxy.example:1080", string(data["HTTPS_PROXY"]))
	require.Equal(t, string(data["HTTP_PROXY"]), string(data["http_proxy"]))
	require.Equal(t, "registry.internal,localhost,127.0.0.1,::1,.svc,.svc.cluster.local", string(data["NO_PROXY"]))
	require.Equal(t, "value", string(data["unrelated"]))

	data, err = reconcileProxySecretData(data, proxy, "", "", false)
	require.NoError(t, err)
	require.Equal(t, "user", string(data[proxyUsernameKey]))
	data, err = reconcileProxySecretData(data, proxy, "", "", true)
	require.NoError(t, err)
	require.NotContains(t, data, proxyUsernameKey)
	require.Equal(t, "http://proxy.example:3128", string(data["HTTP_PROXY"]))
}

func TestLiteralProxyRequiresSiteUpstream(t *testing.T) {
	proxy := &api.OutboundProxy{HTTPProxy: "http://10.0.3.2:8889"}
	require.ErrorContains(t, validateProxyDestination(nil, proxy), "matching upstream")
	require.NoError(t, validateProxyDestination([]api.NetworkDestination{{CIDR: "10.0.3.0/24", Ports: []int32{8889}}}, proxy))
	require.NoError(t, validateProxyDestination(nil, &api.OutboundProxy{HTTPProxy: "http://proxy.internal:8889"}))
}
