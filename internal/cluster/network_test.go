// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"net/netip"
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestSiteNetworkPermissions(t *testing.T) {
	site := &api.GiteaSite{ObjectMeta: metav1.ObjectMeta{UID: "site"}}
	baseline := siteNetworkPolicy(site, "site-ns", "management")
	require.Len(t, baseline.Spec.Ingress, 1)
	require.Len(t, baseline.Spec.Egress, 3)
	site.Spec.Network = api.SiteNetwork{AllowSameSite: true, Egress: api.SiteEgress{AllowInternet: true, Allow: []api.NetworkDestination{
		{CIDR: "10.20.0.0/24", Protocols: []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}, Ports: []api.NetworkPort{{Port: 53}, {Port: 8000, EndPort: 8010}}},
		{CIDR: "fd00::/64", Protocols: []corev1.Protocol{corev1.ProtocolUDP}, AllPorts: true},
	}}}
	policy := siteNetworkPolicy(site, "site-ns", "management")
	require.Equal(t, baseline.Spec.Ingress, policy.Spec.Ingress[:1])
	require.Equal(t, baseline.Spec.Egress, policy.Spec.Egress[:3])
	require.Len(t, policy.Spec.Ingress, 2)
	peer := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{SiteUIDLabel: "site", ComponentLabel: "runtime"}}}
	require.Equal(t, []networkingv1.NetworkPolicyPeer{peer}, policy.Spec.Ingress[1].From)
	require.Equal(t, policy.Spec.Ingress[1].From, policy.Spec.Egress[3].To)
	require.Equal(t, policy.Spec.Ingress[1].Ports, policy.Spec.Egress[3].Ports)
	require.Equal(t, []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP)}, {Protocol: ptr.To(corev1.ProtocolUDP)}}, policy.Spec.Ingress[1].Ports)
	rangeRule := policy.Spec.Egress[6]
	require.Equal(t, "10.20.0.0/24", rangeRule.To[0].IPBlock.CIDR)
	require.Len(t, rangeRule.Ports, 4)
	require.Equal(t, networkingv1.NetworkPolicyPort{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(8000)), EndPort: ptr.To(int32(8010))}, rangeRule.Ports[3])
	require.Equal(t, []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolUDP)}}, policy.Spec.Egress[7].Ports)
	site.Spec.Network.AllowSameSite = false
	require.Equal(t, baseline.Spec.Ingress, siteNetworkPolicy(site, "site-ns", "management").Spec.Ingress)
}

func TestInternetAndExplicitEgress(t *testing.T) {
	egress := api.SiteEgress{AllowInternet: true, InternalCIDRs: []string{"8.8.8.0/24", "2606:4700::/32"}, Allow: []api.NetworkDestination{
		{CIDR: "10.20.0.0/24", Protocols: []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}, Ports: []api.NetworkPort{{Port: 8000, EndPort: 8010}}},
		{CIDR: "8.8.8.8/32", Protocols: []corev1.Protocol{corev1.ProtocolUDP}, AllPorts: true},
	}}
	for _, test := range []struct {
		address  string
		protocol corev1.Protocol
		port     int32
		allowed  bool
	}{
		{"1.1.1.1", corev1.ProtocolTCP, 443, true}, {"2001:4860:4860::8888", corev1.ProtocolUDP, 53, true},
		{"8.8.8.8", corev1.ProtocolTCP, 443, false}, {"8.8.8.8", corev1.ProtocolUDP, 123, true},
		{"2606:4700:4700::1111", corev1.ProtocolTCP, 443, false},
		{"10.20.0.8", corev1.ProtocolTCP, 8000, true}, {"10.20.0.8", corev1.ProtocolUDP, 8010, true},
		{"10.20.0.8", corev1.ProtocolTCP, 8011, false}, {"10.20.1.8", corev1.ProtocolTCP, 8000, false},
	} {
		require.Equal(t, test.allowed, siteAllowsEgress(egress, netip.MustParseAddr(test.address), test.protocol, test.port), "%+v", test)
	}
	for _, address := range []string{"10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1", "192.168.0.1", "198.18.0.1", "224.0.0.1", "240.0.0.1", "::1", "fc00::1", "fe80::1", "ff02::1", "64:ff9b::a00:1", "2001::1", "2002:a00:1::", "2001:db8::1"} {
		require.False(t, siteAllowsEgress(egress, netip.MustParseAddr(address), corev1.ProtocolTCP, 443), address)
	}
	egress.AllowInternet = false
	require.False(t, siteAllowsEgress(egress, netip.MustParseAddr("1.1.1.1"), corev1.ProtocolTCP, 443))
	require.True(t, siteAllowsEgress(egress, netip.MustParseAddr("10.20.0.8"), corev1.ProtocolUDP, 8000))
	require.Empty(t, internetIPBlocks(api.SiteEgress{AllowInternet: true, InternalCIDRs: []string{"0.0.0.0/0", "::/0"}}))
}
