// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2ENetworkPolicy(t *testing.T) {
	cluster := newE2ECluster(t, 5*time.Minute)
	runtime := requireE2ERuntime(t)
	siteNS := cluster.createNamespace(t, "network-site")
	otherNS := cluster.createNamespace(t, "network-other")
	managementNS := cluster.createNamespace(t, "network-management")
	site := &api.GiteaSite{ObjectMeta: metav1.ObjectMeta{UID: siteNS.UID}}
	otherSite := &api.GiteaSite{ObjectMeta: metav1.ObjectMeta{UID: otherNS.UID}}
	otherSite.Spec.Network.Egress.Allow = []api.NetworkDestination{{CIDR: "0.0.0.0/0", Protocols: []corev1.Protocol{corev1.ProtocolTCP}, AllPorts: true}}
	require.NoError(t, cluster.Create(cluster.ctx, siteNetworkPolicy(otherSite, otherNS.Name, managementNS.Name)))
	policy := siteNetworkPolicy(site, siteNS.Name, managementNS.Name)
	require.NoError(t, cluster.Create(cluster.ctx, policy))
	apply := func() {
		t.Helper()
		policy.Spec = siteNetworkPolicy(site, siteNS.Name, managementNS.Name).Spec
		require.NoError(t, cluster.Update(cluster.ctx, policy))
	}
	var pods []*corev1.Pod
	for _, fixture := range []struct {
		name, namespace, role string
		siteUID               types.UID
	}{
		{"runtime", siteNS.Name, "runtime", siteNS.UID},
		{"peer", siteNS.Name, "runtime", siteNS.UID},
		{"other", otherNS.Name, "runtime", otherNS.UID},
		{"gateway", managementNS.Name, "gateway", ""},
		{"manager", managementNS.Name, "manager", ""},
	} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fixture.name, Namespace: fixture.namespace, Labels: map[string]string{ComponentLabel: fixture.role, SiteUIDLabel: string(fixture.siteUID)}},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever,
				TerminationGracePeriodSeconds: ptr.To(int64(1)),
				Containers: []corev1.Container{{Name: "server", Image: "busybox:1.37.0", Command: []string{"sh", "-ec", `mkdir /www
printf network-proof > /www/index.html
for port in 8000 8001 8002 8443 8444; do
  httpd -p "$port" -h /www
done
while :; do nc -u -l -p 8001 -e /bin/cat; done &
sleep 3600`}}},
			},
		}
		if fixture.namespace == siteNS.Name {
			if runtime.runtimeClass != "" {
				pod.Spec.RuntimeClassName = ptr.To(runtime.runtimeClass)
			}
			if runtime.isolation == api.IsolationSysbox {
				pod.Spec.HostUsers = ptr.To(false)
			}
		}
		require.NoError(t, cluster.Create(cluster.ctx, pod))
		pods = append(pods, pod)
	}
	for _, pod := range pods {
		require.Eventually(t, func() bool {
			return cluster.Get(cluster.ctx, client.ObjectKeyFromObject(pod), pod) == nil && pod.Status.Phase == corev1.PodRunning
		}, time.Minute, 200*time.Millisecond, "%s/%s: %+v", pod.Namespace, pod.Name, pod.Status)
	}
	workspace, peer, other, gateway, manager := pods[0], pods[1], pods[2], pods[3], pods[4]
	probe := func(source *corev1.Pod, command string) (string, error) {
		output, err := exec.CommandContext(cluster.ctx, "kubectl", "exec", "-n", source.Namespace, source.Name, "--", "sh", "-c", command).CombinedOutput()
		return string(output), err
	}
	httpProbe := func(source, destination *corev1.Pod, port int, allowed bool) {
		t.Helper()
		expected := "\nblocked"
		if allowed {
			expected = "network-proof\nreachable"
		}
		require.Eventually(t, func() bool {
			output, err := probe(source, "if wget -q -T 1 -O - http://"+net.JoinHostPort(destination.Status.PodIP, strconv.Itoa(port))+"/ 2>/dev/null; then printf '\\nreachable'; else printf '\\nblocked'; fi")
			return err == nil && output == expected
		}, 15*time.Second, 500*time.Millisecond, "%s -> %s:%d allowed=%v", source.Name, destination.Name, port, allowed)
	}
	// Check the server fixture before asserting any denied connection.
	httpProbe(manager, manager, 8001, true)
	httpProbe(gateway, workspace, 8444, true)
	httpProbe(workspace, manager, 8443, true)
	output, err := probe(workspace, "nslookup kubernetes.default.svc.cluster.local")
	require.NoError(t, err, "%s", output)
	httpProbe(gateway, workspace, 8001, false)
	httpProbe(other, workspace, 8444, false)
	httpProbe(workspace, manager, 8001, false)
	httpProbe(workspace, peer, 8001, false)

	site.Spec.Network.AllowSameSite = true
	apply()
	httpProbe(workspace, peer, 8001, true)
	httpProbe(peer, workspace, 8001, true)
	httpProbe(other, workspace, 8001, false)
	httpProbe(workspace, other, 8001, false)
	site.Spec.Network.AllowSameSite = false
	managerAddress := netip.MustParseAddr(manager.Status.PodIP)
	site.Spec.Network.Egress.Allow = []api.NetworkDestination{{
		CIDR:      netip.PrefixFrom(managerAddress, managerAddress.BitLen()).String(),
		Protocols: []corev1.Protocol{corev1.ProtocolUDP}, Ports: []api.NetworkPort{{Port: 8000, EndPort: 8001}},
	}}
	apply()
	udpCommand := fmt.Sprintf("printf network-proof | nc -u -w 1 %s 8001", manager.Status.PodIP)
	require.Eventually(t, func() bool {
		output, err := probe(workspace, udpCommand)
		return err == nil && strings.Contains(output, "network-proof")
	}, 15*time.Second, 500*time.Millisecond, "UDP range must be enforced")
	httpProbe(workspace, manager, 8001, false)
	httpProbe(workspace, peer, 8001, false)
	site.Spec.Network.Egress.Allow[0].Protocols = []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}
	apply()
	httpProbe(workspace, manager, 8000, true)
	httpProbe(workspace, manager, 8001, true)
	httpProbe(workspace, manager, 8002, false)
	site.Spec.Network.Egress.Allow[0].AllPorts = true
	site.Spec.Network.Egress.Allow[0].Ports = nil
	apply()
	httpProbe(workspace, manager, 8002, true)

	// Confirm external reachability independently before testing the switch.
	const internetProbe = "if wget -q -T 3 -O /dev/null http://example.com/ 2>/dev/null; then echo reachable; else echo blocked; fi"
	output, err = probe(manager, internetProbe)
	require.NoError(t, err, "%s", output)
	require.Equal(t, "reachable\n", output, "public test destination must be reachable")
	output, err = probe(workspace, internetProbe)
	require.NoError(t, err, "%s", output)
	require.Equal(t, "blocked\n", output)
	site.Spec.Network.Egress = api.SiteEgress{AllowInternet: true}
	apply()
	require.Eventually(t, func() bool {
		output, err := probe(workspace, internetProbe)
		return err == nil && output == "reachable\n"
	}, 20*time.Second, time.Second, "Internet access")
	httpProbe(workspace, manager, 8002, false)
	httpProbe(gateway, workspace, 8444, true)
	httpProbe(workspace, manager, 8443, true)
	t.Logf("%s: baseline, same-site, cross-site, TCP/UDP ranges, all ports, Internet and private exclusions verified", runtime.isolation)
}
