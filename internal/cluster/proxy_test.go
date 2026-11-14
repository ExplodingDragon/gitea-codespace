// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

func TestProxySiteEgress(t *testing.T) {
	proxy := &api.OutboundProxy{HTTPProxy: "http://10.0.3.2:8889"}
	egress := api.SiteEgress{AllowInternet: true}
	require.ErrorContains(t, validateProxyDestination(egress, proxy), "allowed TCP egress")
	egress.Allow = []api.NetworkDestination{{CIDR: "10.0.3.0/24", Protocols: []corev1.Protocol{corev1.ProtocolTCP}, Ports: []api.NetworkPort{{Port: 8880, EndPort: 8890}}}}
	require.NoError(t, validateProxyDestination(egress, proxy))
	egress.Allow[0].Protocols = []corev1.Protocol{corev1.ProtocolUDP}
	require.Error(t, validateProxyDestination(egress, proxy))
	public := &api.OutboundProxy{HTTPProxy: "http://8.8.8.8:8889"}
	require.NoError(t, validateProxyDestination(egress, public))
	egress.InternalCIDRs = []string{"8.8.8.0/24"}
	require.Error(t, validateProxyDestination(egress, public))
	require.NoError(t, validateProxyDestination(api.SiteEgress{}, &api.OutboundProxy{HTTPProxy: "http://proxy.internal:8889"}))
}
