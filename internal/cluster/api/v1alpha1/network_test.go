// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestSiteNetworkNormalization(t *testing.T) {
	network := SiteNetwork{AllowSameSite: true, Egress: SiteEgress{AllowInternet: true,
		InternalCIDRs: []string{"10.0.0.2/24", " 10.0.0.0/24 ", "2001:4860::1/32"},
		Allow:         []NetworkDestination{{CIDR: " 10.20.0.8/24 ", Protocols: []corev1.Protocol{corev1.ProtocolUDP, corev1.ProtocolTCP}, Ports: []NetworkPort{{Port: 8005, EndPort: 8010}, {Port: 443}, {Port: 8000, EndPort: 8006}, {Port: 443}, {Port: 8011}}}},
	}}
	original := network.DeepCopy()
	got, err := network.Normalized()
	require.NoError(t, err)
	require.Equal(t, *original, network)
	require.Equal(t, []string{"10.0.0.0/24", "2001:4860::/32"}, got.Egress.InternalCIDRs)
	require.Equal(t, NetworkDestination{CIDR: "10.20.0.0/24", Protocols: []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}, Ports: []NetworkPort{{Port: 443}, {Port: 8000, EndPort: 8011}}}, got.Egress.Allow[0])
	again, err := got.Normalized()
	require.NoError(t, err)
	require.Equal(t, got, again)
}

func TestSiteNetworkValidation(t *testing.T) {
	base := SiteNetwork{Egress: SiteEgress{Allow: []NetworkDestination{{CIDR: "10.20.0.0/24", Protocols: []corev1.Protocol{corev1.ProtocolTCP}, Ports: []NetworkPort{{Port: 443}}}}}}
	for _, test := range []struct {
		name   string
		change func(*SiteNetwork)
		valid  bool
	}{
		{"single port", func(*SiteNetwork) {}, true},
		{"all UDP ports", func(n *SiteNetwork) {
			n.Egress.Allow[0].AllPorts = true
			n.Egress.Allow[0].Ports = nil
			n.Egress.Allow[0].Protocols = []corev1.Protocol{corev1.ProtocolUDP}
		}, true},
		{"IPv6", func(n *SiteNetwork) { n.Egress.Allow[0].CIDR = "fd00::/64" }, true},
		{"missing ports", func(n *SiteNetwork) { n.Egress.Allow[0].Ports = nil }, false},
		{"ambiguous all ports", func(n *SiteNetwork) { n.Egress.Allow[0].AllPorts = true }, false},
		{"empty protocols", func(n *SiteNetwork) { n.Egress.Allow[0].Protocols = nil }, false},
		{"invalid protocol", func(n *SiteNetwork) { n.Egress.Allow[0].Protocols = []corev1.Protocol{"ICMP"} }, false},
		{"inverted range", func(n *SiteNetwork) { n.Egress.Allow[0].Ports[0].EndPort = 1 }, false},
		{"out of range", func(n *SiteNetwork) { n.Egress.Allow[0].Ports[0].EndPort = 65536 }, false},
		{"zero port", func(n *SiteNetwork) { n.Egress.Allow[0].Ports[0].Port = 0 }, false},
		{"invalid destination", func(n *SiteNetwork) { n.Egress.Allow[0].CIDR = "example.com" }, false},
		{"invalid internal network", func(n *SiteNetwork) { n.Egress.InternalCIDRs = []string{"10.0.0.1"} }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			network := base.DeepCopy()
			test.change(network)
			_, err := network.Normalized()
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
