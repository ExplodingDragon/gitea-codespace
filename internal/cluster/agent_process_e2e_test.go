// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2EAgentProcesses(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_KUBERNETES_AGENT") != "1" {
		t.Skip("run make test-kubernetes-agent after importing the Docker platform image")
	}
	binary, err := filepath.Abs("../../bin/agent.test")
	require.NoError(t, err)
	_, err = os.Stat(binary)
	require.NoError(t, err)
	platformImage := os.Getenv("CODESPACE_TEST_PLATFORM_IMAGE")
	require.NotEmpty(t, platformImage, "CODESPACE_TEST_PLATFORM_IMAGE must name the manually imported platform image")
	runtimeIsolation := os.Getenv("CODESPACE_TEST_RUNTIME_ISOLATION")
	runtimeClass := os.Getenv("CODESPACE_TEST_RUNTIME_CLASS")
	storageClass := os.Getenv("CODESPACE_TEST_STORAGE_CLASS")
	require.Contains(t, []string{"kata", "sysbox"}, runtimeIsolation)
	require.NotEmpty(t, runtimeClass)
	require.NotEmpty(t, storageClass)
	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	c, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agent-e2e-" + uuid.NewString()}}
	require.NoError(t, c.Create(ctx, ns))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		require.NoError(t, client.IgnoreNotFound(c.Delete(cleanup, ns, client.Preconditions{UID: &ns.UID})))
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(c.Get(cleanup, client.ObjectKeyFromObject(ns), &corev1.Namespace{}))
		}, time.Minute, 200*time.Millisecond)
	})
	volume := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns.Name},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: ptr.To(storageClass), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")}}},
	}
	require.NoError(t, c.Create(ctx, volume))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns.Name},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(runtimeClass), AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "runtime", Image: platformImage, ImagePullPolicy: corev1.PullNever,
				Command:      []string{"sleep", "3600"},
				Resources:    corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/codespace"}},
			}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: volume.Name}}}},
		},
	}
	if runtimeIsolation == "kata" {
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	} else {
		pod.Spec.HostUsers = ptr.To(false)
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0))}
	}
	require.NoError(t, c.Create(ctx, pod))
	require.Eventually(t, func() bool {
		return c.Get(ctx, client.ObjectKeyFromObject(pod), pod) == nil && pod.Status.Phase == corev1.PodRunning
	}, time.Minute, 200*time.Millisecond)
	command := exec.CommandContext(ctx, "kubectl", "exec", "-n", ns.Name, "agent", "--", "sh", "-c", "test -x /usr/local/bin/gitea-codespace && command -v dockerd git ssh useradd groupadd >/dev/null")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	command = exec.CommandContext(ctx, "kubectl", "cp", binary, ns.Name+"/agent:/agent.test")
	output, err = command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	for _, args := range [][]string{
		{"env", "CODESPACE_TEST_AGENT_PROCESS=1", "unshare", "--pid", "--fork", "--mount-proc", "/agent.test", "-test.run=^TestAgentPID1Supervision$", "-test.v"},
		{"env", "CODESPACE_TEST_AGENT_DOCKER=1", "/agent.test", "-test.run=^TestAgentDockerSupervision$", "-test.v"},
	} {
		command := exec.CommandContext(ctx, "kubectl", append([]string{"exec", "-n", ns.Name, "agent", "--"}, args...)...)
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Log(string(output))
	}
}
