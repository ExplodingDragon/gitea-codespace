// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestKubernetesE2EDockerPersistence verifies the runtime/storage combination,
// independently of the Manager's lifecycle and access implementations.
func TestKubernetesE2EDockerPersistence(t *testing.T) {
	requireE2E(t)
	devContainerTest, err := filepath.Abs(requireE2EEnvironment(t, "CODESPACE_E2E_DEVCONTAINER_TEST_BINARY"))
	require.NoError(t, err)
	_, err = os.Stat(devContainerTest)
	require.NoError(t, err)
	runtime := requireE2ERuntime(t)
	cluster := newE2ECluster(t, 35*time.Minute)
	ns := cluster.createNamespace(t, "runtime-e2e")
	volumeMode := corev1.PersistentVolumeFilesystem
	if runtime.isolation == "kata" {
		volumeMode = corev1.PersistentVolumeBlock
	}
	volume := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns.Name},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, VolumeMode: ptr.To(volumeMode), StorageClassName: ptr.To(runtime.storageClass), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("6Gi")}}},
	}
	require.NoError(t, cluster.Create(cluster.ctx, volume))
	dockerArguments := []string{"--host=unix:///run/docker.sock", "--data-root=/var/lib/codespace/docker", "--feature=containerd-snapshotter=true"}
	startDocker := `mtu="$(ip -o link show up | awk '$2 !~ /^lo:/ {for (i = 1; i <= NF; i++) if ($i == "mtu") print $(i + 1)}' | sort -n | head -n 1)"
[ -n "$mtu" ] || { echo "runtime has no usable network interface MTU" >&2; exit 1; }
exec dockerd "$@" --mtu="$mtu"`
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "docker", Namespace: ns.Name},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(runtime.runtimeClass), AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever, TerminationGracePeriodSeconds: ptr.To(int64(60)),
			Containers: []corev1.Container{{
				Name: "docker", Image: runtime.platformImage, ImagePullPolicy: corev1.PullNever,
				Command:      []string{"sh", "-ec"},
				Args:         append([]string{startDocker, "dockerd"}, dockerArguments...),
				Resources:    corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/codespace"}},
			}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: volume.Name}}}},
		},
	}
	if runtime.isolation == "kata" {
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
` + startDocker, "dockerd", dockerArguments[0], dockerArguments[1], dockerArguments[2]}
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
		command := exec.CommandContext(cluster.ctx, "kubectl", commandArgs...)
		command.Stdin = input
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("kubectl exec: %w: %s", err, output)
		}
		return strings.TrimSpace(string(output)), nil
	}
	awaitDocker := func() {
		t.Helper()
		ready := assert.Eventually(t, func() bool {
			if cluster.Get(cluster.ctx, client.ObjectKeyFromObject(pod), pod) != nil || pod.Status.Phase != corev1.PodRunning {
				return false
			}
			_, err := execPod(nil, "docker", "info")
			return err == nil
		}, time.Minute, 500*time.Millisecond)
		if !ready {
			status, _ := exec.CommandContext(cluster.ctx, "kubectl", "get", "pod", "-n", ns.Name, pod.Name, "-o", "yaml").CombinedOutput()
			logs, _ := exec.CommandContext(cluster.ctx, "kubectl", "logs", "-n", ns.Name, pod.Name, "--all-containers", "--tail=200").CombinedOutput()
			t.Fatalf("Docker did not become ready\nPod:\n%s\nLogs:\n%s", status, logs)
		}
	}
	require.NoError(t, cluster.Create(cluster.ctx, pod))
	awaitDocker()
	output, err := execPod(nil, "docker", "info", "--format", "{{.Driver}} {{.DockerRootDir}} {{json .DriverStatus}}")
	require.NoError(t, err)
	require.Contains(t, output, "overlayfs /var/lib/codespace/docker")
	require.Contains(t, output, "io.containerd.snapshotter.v1")
	_, err = execPod(nil, "docker", "pull", "busybox:1.37.0")
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
	require.NoError(t, cluster.Delete(cluster.ctx, pod, client.Preconditions{UID: &oldUID}))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(cluster.Get(cluster.ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
	}, time.Minute, 200*time.Millisecond)
	pod = prototype
	require.NoError(t, cluster.Create(cluster.ctx, pod))
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

	command := exec.CommandContext(cluster.ctx, "kubectl", "cp", devContainerTest, ns.Name+"/docker:/devcontainer.test")
	copyOutput, err := command.CombinedOutput()
	require.NoError(t, err, "%s", copyOutput)
	_, err = execPod(nil, "sh", "-ec", `
mkdir -p /var/lib/codespace/e2e-home /var/lib/codespace/e2e-tmp
chown -R 1000:1000 /var/lib/codespace/e2e-home /var/lib/codespace/e2e-tmp
chmod 0666 /run/docker.sock
exec setpriv --reuid=1000 --regid=1000 --clear-groups \
  env HOME=/var/lib/codespace/e2e-home USER=codespace TMPDIR=/var/lib/codespace/e2e-tmp CODESPACE_E2E=1 \
  /devcontainer.test -test.run '^TestDockerE2E' -test.v -test.timeout=25m`)
	require.NoError(t, err)
}
