// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2EAgentProcesses(t *testing.T) {
	requireE2E(t)
	binary, err := filepath.Abs(requireE2EEnvironment(t, "CODESPACE_E2E_AGENT_TEST_BINARY"))
	require.NoError(t, err)
	_, err = os.Stat(binary)
	require.NoError(t, err)
	runtime := requireE2ERuntime(t)
	cluster := newE2ECluster(t, 4*time.Minute)
	ns := cluster.createNamespace(t, "agent-e2e")
	volume := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns.Name},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: ptr.To(runtime.storageClass), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")}}},
	}
	require.NoError(t, cluster.Create(cluster.ctx, volume))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns.Name},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(runtime.runtimeClass), AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "runtime", Image: runtime.platformImage, ImagePullPolicy: corev1.PullNever,
				Command:      []string{"sleep", "3600"},
				Resources:    corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/codespace"}},
			}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: volume.Name}}}},
		},
	}
	if runtime.isolation == "kata" {
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	} else {
		pod.Spec.HostUsers = ptr.To(false)
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0))}
	}
	require.NoError(t, cluster.Create(cluster.ctx, pod))
	require.Eventually(t, func() bool {
		return cluster.Get(cluster.ctx, client.ObjectKeyFromObject(pod), pod) == nil && pod.Status.Phase == corev1.PodRunning
	}, time.Minute, 200*time.Millisecond)
	command := exec.CommandContext(cluster.ctx, "kubectl", "exec", "-n", ns.Name, "agent", "--", "sh", "-c", "test -x /usr/local/bin/gitea-codespace && command -v dockerd git ssh useradd groupadd >/dev/null")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	command = exec.CommandContext(cluster.ctx, "kubectl", "cp", binary, ns.Name+"/agent:/agent.test")
	output, err = command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	for _, args := range [][]string{
		{"env", "CODESPACE_E2E=1", "unshare", "--pid", "--fork", "--mount-proc", "/agent.test", "-test.run=^TestAgentE2EPID1Supervision$", "-test.v"},
		{"env", "CODESPACE_E2E=1", "/agent.test", "-test.run=^TestAgentE2EDockerSupervision$", "-test.v"},
	} {
		command := exec.CommandContext(cluster.ctx, "kubectl", append([]string{"exec", "-n", ns.Name, "agent", "--"}, args...)...)
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
}
