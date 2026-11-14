// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"net/netip"
	"slices"
	"strings"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// Conservative exclusions from the IANA special-purpose address registries.
// IPv6 Internet access is limited to global unicast, excluding transition and
// special-purpose ranges. Site-specific internal networks extend this list.
var internetExcludedCIDRs = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
}

func internetIPBlocks(egress api.SiteEgress) []networkingv1.IPBlock {
	if !egress.AllowInternet {
		return nil
	}
	exclusions := append(slices.Clone(internetExcludedCIDRs), egress.InternalCIDRs...)
	var blocks []networkingv1.IPBlock
	for _, cidr := range []string{"0.0.0.0/0", "2000::/3"} {
		base := netip.MustParsePrefix(cidr)
		block := networkingv1.IPBlock{CIDR: cidr}
		covered := false
		for _, raw := range exclusions {
			prefix := netip.MustParsePrefix(strings.TrimSpace(raw)).Masked()
			if prefix.Addr().BitLen() != base.Addr().BitLen() {
				continue
			}
			if prefix.Bits() <= base.Bits() && prefix.Contains(base.Addr()) {
				covered = true
				break
			}
			if base.Contains(prefix.Addr()) {
				block.Except = append(block.Except, prefix.String())
			}
		}
		if !covered {
			slices.Sort(block.Except)
			block.Except = slices.Compact(block.Except)
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// siteAllowsEgress checks explicit IP permissions, also used when saving literal-IP proxies.
// Platform component and same-site permissions are selected by Pod identity instead.
func siteAllowsEgress(egress api.SiteEgress, address netip.Addr, protocol corev1.Protocol, port int32) bool {
	address = address.Unmap()
	if protocol != corev1.ProtocolTCP && protocol != corev1.ProtocolUDP {
		return false
	}
	for _, block := range internetIPBlocks(egress) {
		if !netip.MustParsePrefix(block.CIDR).Contains(address) {
			continue
		}
		if !slices.ContainsFunc(block.Except, func(cidr string) bool { return netip.MustParsePrefix(cidr).Contains(address) }) {
			return true
		}
	}
	for _, rule := range egress.Allow {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(rule.CIDR))
		if err != nil || !prefix.Contains(address) || !slices.Contains(rule.Protocols, protocol) {
			continue
		}
		if rule.AllPorts {
			return true
		}
		for _, allowed := range rule.Ports {
			if port >= allowed.Port && port <= max(allowed.Port, allowed.EndPort) {
				return true
			}
		}
	}
	return false
}

func siteNetworkPolicy(site *api.GiteaSite, namespace, managementNamespace string) *networkingv1.NetworkPolicy {
	management := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": managementNamespace}}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "codespace", Namespace: namespace, Labels: map[string]string{SiteUIDLabel: string(site.UID)}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: management, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{ComponentLabel: "gateway"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(8444))}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: management, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{ComponentLabel: "manager"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(8443))}},
			}, {
				To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: management, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{ComponentLabel: "cache"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromString(cacheHTTPSPortName))}},
			}, {
				To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))}, {Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(53))}},
			}},
		},
	}
	allPorts := []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP)}, {Protocol: ptr.To(corev1.ProtocolUDP)}}
	if site.Spec.Network.AllowSameSite {
		peers := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{SiteUIDLabel: string(site.UID), ComponentLabel: "runtime"}}}}
		policy.Spec.Ingress = append(policy.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{From: peers, Ports: allPorts})
		policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: peers, Ports: allPorts})
	}
	for _, block := range internetIPBlocks(site.Spec.Network.Egress) {
		policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &block}}, Ports: allPorts,
		})
	}
	for _, target := range site.Spec.Network.Egress.Allow {
		prefix := netip.MustParsePrefix(strings.TrimSpace(target.CIDR)).Masked()
		rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: prefix.String()}}}}
		for _, protocol := range target.Protocols {
			if target.AllPorts {
				rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: ptr.To(protocol)})
				continue
			}
			for _, port := range target.Ports {
				entry := networkingv1.NetworkPolicyPort{Protocol: ptr.To(protocol), Port: ptr.To(intstr.FromInt32(port.Port))}
				if port.EndPort != 0 {
					entry.EndPort = ptr.To(port.EndPort)
				}
				rule.Ports = append(rule.Ports, entry)
			}
		}
		policy.Spec.Egress = append(policy.Spec.Egress, rule)
	}
	return policy
}
