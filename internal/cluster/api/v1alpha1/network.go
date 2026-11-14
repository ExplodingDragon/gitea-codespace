// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Normalized validates network permissions and returns a canonical copy for storage.
func (n SiteNetwork) Normalized() (SiteNetwork, error) {
	result := n.DeepCopy()
	if len(result.Egress.InternalCIDRs) > 128 || len(result.Egress.Allow) > 128 {
		return SiteNetwork{}, fmt.Errorf("network allows at most 128 internal CIDRs and 128 destination rules")
	}
	for i, raw := range result.Egress.InternalCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil || prefix.Addr().Is4In6() {
			return SiteNetwork{}, fmt.Errorf("internal CIDR %q must be an IPv4 or IPv6 prefix", raw)
		}
		result.Egress.InternalCIDRs[i] = prefix.Masked().String()
	}
	slices.Sort(result.Egress.InternalCIDRs)
	result.Egress.InternalCIDRs = slices.Compact(result.Egress.InternalCIDRs)
	for i := range result.Egress.Allow {
		rule := &result.Egress.Allow[i]
		prefix, err := netip.ParsePrefix(strings.TrimSpace(rule.CIDR))
		if err != nil || prefix.Addr().Is4In6() {
			return SiteNetwork{}, fmt.Errorf("network rule %d requires an IPv4 or IPv6 CIDR", i+1)
		}
		rule.CIDR = prefix.Masked().String()
		if len(rule.Protocols) == 0 || len(rule.Protocols) > 2 {
			return SiteNetwork{}, fmt.Errorf("network rule %d requires TCP, UDP, or both protocols", i+1)
		}
		for _, protocol := range rule.Protocols {
			if protocol != corev1.ProtocolTCP && protocol != corev1.ProtocolUDP {
				return SiteNetwork{}, fmt.Errorf("network rule %d has unsupported protocol %q", i+1, protocol)
			}
		}
		slices.Sort(rule.Protocols)
		rule.Protocols = slices.Compact(rule.Protocols)
		if len(rule.Ports) > 128 || rule.AllPorts != (len(rule.Ports) == 0) {
			return SiteNetwork{}, fmt.Errorf("network rule %d requires either all ports or 1 to 128 port entries", i+1)
		}
		for j := range rule.Ports {
			port := &rule.Ports[j]
			if port.Port < 1 || port.Port > 65535 || (port.EndPort != 0 && (port.EndPort < port.Port || port.EndPort > 65535)) {
				return SiteNetwork{}, fmt.Errorf("network rule %d ports must be between 1 and 65535, with endPort at least port", i+1)
			}
			if port.EndPort == 0 {
				port.EndPort = port.Port
			}
		}
		slices.SortFunc(rule.Ports, func(a, b NetworkPort) int { return cmp.Compare(a.Port, b.Port) })
		merged := rule.Ports[:0]
		for _, port := range rule.Ports {
			if len(merged) > 0 && port.Port <= merged[len(merged)-1].EndPort+1 {
				merged[len(merged)-1].EndPort = max(merged[len(merged)-1].EndPort, port.EndPort)
			} else {
				merged = append(merged, port)
			}
		}
		for j := range merged {
			if merged[j].EndPort == merged[j].Port {
				merged[j].EndPort = 0
			}
		}
		rule.Ports = merged
	}
	return *result, nil
}
