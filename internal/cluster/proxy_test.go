// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestUpdateProxySecretData(t *testing.T) {
	proxy := &api.OutboundProxy{HTTPProxy: "http://user:password@proxy.example:3128", SOCKSProxy: "socks5h://socks-user:socks-password@proxy.example:1080", NoProxy: []string{"registry.internal"}, CABundle: "ca"}
	data := updateProxySecretData(map[string][]byte{"unrelated": []byte("value")}, proxy)
	require.Equal(t, proxy.HTTPProxy, string(data["HTTP_PROXY"]))
	require.Equal(t, proxy.SOCKSProxy, string(data["HTTPS_PROXY"]))
	require.Equal(t, string(data["HTTP_PROXY"]), string(data["http_proxy"]))
	require.Equal(t, "registry.internal,localhost,127.0.0.1,::1,.svc,.svc.cluster.local", string(data["NO_PROXY"]))
	require.Equal(t, "value", string(data["unrelated"]))

	data = updateProxySecretData(data, nil)
	require.NotContains(t, data, "HTTP_PROXY")
	require.NotContains(t, data, proxyCAKey)
	require.Equal(t, "value", string(data["unrelated"]))
}

func TestLiteralProxyRequiresSiteUpstream(t *testing.T) {
	proxy := &api.OutboundProxy{HTTPProxy: "http://10.0.3.2:8889"}
	require.ErrorContains(t, validateProxyDestination(nil, proxy), "matching upstream")
	require.NoError(t, validateProxyDestination([]api.NetworkDestination{{CIDR: "10.0.3.0/24", Ports: []int32{8889}}}, proxy))
	require.NoError(t, validateProxyDestination(nil, &api.OutboundProxy{HTTPProxy: "http://proxy.internal:8889"}))
}
