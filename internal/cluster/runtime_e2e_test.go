// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
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

// TestKubernetesE2EDockerPersistence verifies the runtime/storage combination,
// independently of the Manager's lifecycle and access implementations.
func TestKubernetesE2EDockerPersistence(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_KUBERNETES_RUNTIME") != "1" {
		t.Skip("run make test-kubernetes-runtime with the manually exported BusyBox Docker archive")
	}
	archive, err := os.Open(os.Getenv("CODESPACE_TEST_DOCKER_ARCHIVE"))
	require.NoError(t, err)
	defer func() { require.NoError(t, archive.Close()) }()
	runtimeIsolation := os.Getenv("CODESPACE_TEST_RUNTIME_ISOLATION")
	runtimeClass := os.Getenv("CODESPACE_TEST_RUNTIME_CLASS")
	storageClass := os.Getenv("CODESPACE_TEST_STORAGE_CLASS")
	platformImage := os.Getenv("CODESPACE_TEST_PLATFORM_IMAGE")
	require.Contains(t, []string{"kata", "sysbox"}, runtimeIsolation)
	require.NotEmpty(t, runtimeClass)
	require.NotEmpty(t, storageClass)
	require.NotEmpty(t, platformImage)
	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	c, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime-e2e-" + uuid.NewString()}}
	require.NoError(t, c.Create(ctx, ns))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		require.NoError(t, client.IgnoreNotFound(c.Delete(cleanup, ns, client.Preconditions{UID: &ns.UID})))
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(c.Get(cleanup, client.ObjectKeyFromObject(ns), &corev1.Namespace{}))
		}, time.Minute, 200*time.Millisecond)
	})
	volumeMode := corev1.PersistentVolumeFilesystem
	if runtimeIsolation == "kata" {
		volumeMode = corev1.PersistentVolumeBlock
	}
	volume := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns.Name},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, VolumeMode: ptr.To(volumeMode), StorageClassName: ptr.To(storageClass), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")}}},
	}
	require.NoError(t, c.Create(ctx, volume))
	dockerArguments := []string{"--host=unix:///run/docker.sock", "--data-root=/var/lib/codespace/docker", "--feature=containerd-snapshotter=true"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "docker", Namespace: ns.Name},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(runtimeClass), AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever, TerminationGracePeriodSeconds: ptr.To(int64(60)),
			Containers: []corev1.Container{{
				Name: "docker", Image: platformImage, ImagePullPolicy: corev1.PullNever,
				Command:      append([]string{"dockerd"}, dockerArguments...),
				Resources:    corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/codespace"}},
			}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: volume.Name}}}},
		},
	}
	if runtimeIsolation == "kata" {
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		pod.Spec.Containers[0].Command = []string{"sh", "-ec"}
		pod.Spec.Containers[0].Args = []string{`filesystem="$(blkid -p -o value -s TYPE /dev/codespace-data || true)"
if [ -z "$filesystem" ]; then
  mkfs.ext4 -F -m 0 /dev/codespace-data
elif [ "$filesystem" != ext4 ]; then
  echo "unexpected filesystem: $filesystem" >&2
  exit 1
fi
mkdir -p /var/lib/codespace
mount -t ext4 -o noatime /dev/codespace-data /var/lib/codespace
exec dockerd "$@"`, "dockerd", dockerArguments[0], dockerArguments[1], dockerArguments[2]}
		pod.Spec.Containers[0].VolumeMounts = nil
		pod.Spec.Containers[0].VolumeDevices = []corev1.VolumeDevice{{Name: "data", DevicePath: "/dev/codespace-data"}}
	} else {
		pod.Spec.HostUsers = ptr.To(false)
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0))}
	}
	prototype := pod.DeepCopy()
	execPod := func(input io.Reader, args ...string) (string, error) {
		commandArgs := []string{"exec", "-n", ns.Name, pod.Name}
		if input != nil {
			commandArgs = append(commandArgs, "-i")
		}
		commandArgs = append(append(commandArgs, "--"), args...)
		command := exec.CommandContext(ctx, "kubectl", commandArgs...)
		command.Stdin = input
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("kubectl exec: %w: %s", err, output)
		}
		return strings.TrimSpace(string(output)), nil
	}
	awaitDocker := func() {
		t.Helper()
		require.Eventually(t, func() bool {
			if c.Get(ctx, client.ObjectKeyFromObject(pod), pod) != nil || pod.Status.Phase != corev1.PodRunning {
				return false
			}
			_, err := execPod(nil, "docker", "info")
			return err == nil
		}, time.Minute, 500*time.Millisecond)
	}
	require.NoError(t, c.Create(ctx, pod))
	awaitDocker()
	output, err := execPod(nil, "docker", "info", "--format", "{{.Driver}} {{.DockerRootDir}} {{json .DriverStatus}}")
	require.NoError(t, err)
	require.Contains(t, output, "overlayfs /var/lib/codespace/docker")
	require.Contains(t, output, "io.containerd.snapshotter.v1")
	_, err = execPod(archive, "docker", "load")
	require.NoError(t, err)
	containerID, err := execPod(nil, "docker", "run", "--name", "persistent", "--pull=never", "-v", "/var/lib/codespace/workspaces:/workspace", "-d", "busybox:1.37.0", "sleep", "3600")
	require.NoError(t, err)
	_, err = execPod(bytes.NewBufferString("workspace-proof"), "docker", "exec", "-i", "persistent", "sh", "-c", "cat > /workspace/proof")
	require.NoError(t, err)
	_, err = execPod(bytes.NewBufferString("layer-proof"), "docker", "exec", "-i", "persistent", "sh", "-c", "cat > /proof")
	require.NoError(t, err)
	_, err = execPod(nil, "docker", "stop", "persistent")
	require.NoError(t, err)
	oldUID := pod.UID
	require.NoError(t, c.Delete(ctx, pod, client.Preconditions{UID: &oldUID}))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
	}, time.Minute, 200*time.Millisecond)
	pod = prototype
	require.NoError(t, c.Create(ctx, pod))
	require.NotEqual(t, oldUID, pod.UID)
	awaitDocker()
	output, err = execPod(nil, "docker", "inspect", "--format", "{{.Id}}", "persistent")
	require.NoError(t, err)
	require.Equal(t, containerID, output)
	_, err = execPod(nil, "docker", "start", "persistent")
	require.NoError(t, err)
	output, err = execPod(nil, "docker", "exec", "persistent", "cat", "/workspace/proof", "/proof")
	require.NoError(t, err)
	require.Equal(t, "workspace-prooflayer-proof", output)
}
