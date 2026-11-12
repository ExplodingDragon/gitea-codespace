#!/usr/bin/env bash

set -euo pipefail

K3S_VERSION=v1.36.5+k3s1
K3S_INSTALL_COMMIT=3461a87f36257bee62fafc980e43ef8b23514145
source "$(dirname "${BASH_SOURCE[0]}")/kata-kernel.version"
KATA_USBIP_RUNTIME_CLASS=kata-qemu-usbip-runtime-rs
KATA_USBIP_KERNEL_PATH=/opt/kata/share/kernels/codespace-usbip-vmlinuz
RAWFILE_VERSION=0.15.3
SYSBOX_VERSION=0.7.1
SYSBOX_SHA256=9d6d5484f980d0a17f86c492c1262015c2afb66280bdb97215b79fde6a0261c5

runtime=${1:-}
if [[ $runtime != kata && $runtime != sysbox && $runtime != native ]]; then
	echo "usage: $0 <kata|sysbox|native>" >&2
	exit 2
fi

for command_name in curl helm kubectl sha256sum; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		echo "required command is unavailable: $command_name" >&2
		exit 1
	fi
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if [[ $runtime == sysbox ]]; then
	echo "Installing Sysbox $SYSBOX_VERSION"
	package="$work/sysbox.deb"
	curl -fsSL "https://github.com/nestybox/sysbox/releases/download/v$SYSBOX_VERSION/sysbox-ce_$SYSBOX_VERSION.linux_amd64.deb" -o "$package"
	printf '%s  %s\n' "$SYSBOX_SHA256" "$package" | sha256sum --check
	sudo apt-get update
	sudo apt-get install -y "$package"
	sudo systemctl is-active --quiet sysbox-mgr sysbox-fs
elif [[ $runtime == kata ]]; then
	echo "Checking Kata hardware virtualization"
	test -c /dev/kvm
	sudo chmod 0666 /dev/kvm
	test -r /dev/kvm
	test -w /dev/kvm
	if [[ -z ${CODESPACE_E2E_KATA_KERNEL_DIR:-} || ! -r ${CODESPACE_E2E_KATA_KERNEL_DIR}/vmlinuz ]]; then
		echo "CODESPACE_E2E_KATA_KERNEL_DIR must contain the CI-built USB/IP guest kernel" >&2
		exit 1
	fi
fi

echo "Installing k3s $K3S_VERSION"
installer="$work/install-k3s.sh"
curl -fsSL "https://raw.githubusercontent.com/k3s-io/k3s/$K3S_INSTALL_COMMIT/install.sh" -o "$installer"
sudo env \
	INSTALL_K3S_VERSION="$K3S_VERSION" \
	INSTALL_K3S_EXEC='server --disable=servicelb --disable=traefik --write-kubeconfig-mode=0644' \
	sh "$installer"

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl wait --for=condition=Ready node --all --timeout=3m

if [[ $runtime == sysbox ]]; then
	echo "Registering Sysbox with k3s containerd"
	sudo install -d -m 0755 /var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d
	sudo tee /var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/codespace-sysbox.toml >/dev/null <<'EOF'
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.sysbox-runc]
  runtime_type = "io.containerd.runc.v2"
  pod_annotations = ["io.kubernetes.cri-o.userns-mode", "io.kubernetes.cri-o.Devices"]

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.sysbox-runc.options]
  BinaryName = "/usr/bin/sysbox-runc"
  SystemdCgroup = true
EOF
	sudo systemctl restart k3s
	kubectl wait --for=condition=Ready node --all --timeout=3m
	kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: sysbox-runc
  labels:
    codespace.gitea.dev/isolation: sysbox
handler: sysbox-runc
EOF
	kubectl get runtimeclass/sysbox-runc >/dev/null
	kubectl wait --for=create storageclass/local-path --timeout=2m
	exit 0
fi

if [[ $runtime == native ]]; then
	kubectl wait --for=create storageclass/local-path --timeout=2m
	exit 0
fi

echo "Installing Kata Containers $KATA_VERSION"
kata_values="$work/kata-usbip-values.yaml"
cat >"$kata_values" <<EOF
customRuntimes:
  enabled: true
  runtimes:
    qemu-usbip-runtime-rs:
      baseConfig: qemu-runtime-rs
      dropIn: |
        [hypervisor.qemu]
        kernel = "$KATA_USBIP_KERNEL_PATH"
      runtimeClass: |
        apiVersion: node.k8s.io/v1
        kind: RuntimeClass
        metadata:
          name: $KATA_USBIP_RUNTIME_CLASS
          labels:
            codespace.gitea.dev/isolation: kata
        handler: $KATA_USBIP_RUNTIME_CLASS
        scheduling:
          nodeSelector:
            katacontainers.io/kata-runtime: "true"
EOF
helm upgrade --install kata-deploy \
	oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy \
	--version "$KATA_VERSION" \
	--namespace kube-system \
	--set k8sDistribution=k3s \
	--set shims.disableAll=true \
	--set shims.qemu-runtime-rs.enabled=true \
	--values "$kata_values" \
	--wait \
	--timeout 10m
containerd_kata_config=/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/kata-deploy.toml
if ! sudo awk -v handler="$KATA_USBIP_RUNTIME_CLASS" '
	index($0, ".containerd.runtimes." handler "]") > 0 {
		in_handler = 1
		next
	}
	in_handler && /^\[/ {
		in_handler = 0
	}
	in_handler {
		line = $0
		gsub(/^[[:space:]]+|[[:space:]]+$/, "", line)
		if (line == "privileged_without_host_devices = true") {
			found = 1
		}
	}
	END { exit !found }
' "$containerd_kata_config"; then
	echo "Kata handler $KATA_USBIP_RUNTIME_CLASS is missing privileged_without_host_devices=true" >&2
	exit 1
fi
kubectl get runtimeclass/kata-qemu-runtime-rs >/dev/null
kubectl label runtimeclass/kata-qemu-runtime-rs codespace.gitea.dev/isolation=kata --overwrite
kubectl get "runtimeclass/$KATA_USBIP_RUNTIME_CLASS" >/dev/null
kubectl label "runtimeclass/$KATA_USBIP_RUNTIME_CLASS" codespace.gitea.dev/isolation=kata --overwrite
sudo install -D -m 0644 "$CODESPACE_E2E_KATA_KERNEL_DIR/vmlinuz" "$KATA_USBIP_KERNEL_PATH"
sudo install -D -m 0644 "$CODESPACE_E2E_KATA_KERNEL_DIR/config" \
	"${KATA_USBIP_KERNEL_PATH%/*}/codespace-usbip-config"

echo "Installing RawFile LocalPV $RAWFILE_VERSION"
helm repo add rawfile-localpv https://openebs.github.io/rawfile-localpv
helm repo update rawfile-localpv
cat >"$work/rawfile-values.yaml" <<'EOF'
global:
  analytics:
    enabled: false
capabilities:
  snapshots:
    enabled: false
  resize:
    enabled: false
  apiServer:
    enabled: false
metrics:
  enabled: false
storageClasses:
  - name: rawfile-localpv
    enabled: true
    volumeBindingMode: WaitForFirstConsumer
    isDefault: false
    allowVolumeExpansion: true
    reclaimPolicy: Delete
    fsType: ext4
    thinProvision: "true"
EOF
helm upgrade --install rawfile-localpv rawfile-localpv/rawfile-localpv \
	--version "$RAWFILE_VERSION" \
	--namespace openebs \
	--create-namespace \
	--values "$work/rawfile-values.yaml" \
	--wait \
	--timeout 5m
kubectl get storageclass/rawfile-localpv >/dev/null
