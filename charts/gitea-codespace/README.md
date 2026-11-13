# Gitea Codespace Helm chart

This chart installs Codespace Manager and its administration service. Use the
administration page to connect Gitea and configure workspace environments,
Gateway, and Cache components.

## Requirements

- Kubernetes 1.31 or later
- Helm with OCI registry support
- kubectl and OpenSSL for the installation commands
- Gitea with Codespace support
- A compatible StorageClass
- A verified Kata or Sysbox RuntimeClass when using that runtime

Test the runtime and StorageClass together on every workspace node pool because
a successful Pod start does not verify nested containers or persistent storage.

## Choose a workspace runtime

| Runtime         | Isolation       | Workspace volume | Preparation                                                                                       |
| --------------- | --------------- | ---------------- | ------------------------------------------------------------------------------------------------- |
| Kata Containers | Virtual machine | Raw block PVC    | [Install Kata](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md) |
| Sysbox          | User namespace  | Filesystem PVC   | [Install Sysbox](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md)   |
| Native          | Node kernel     | Filesystem PVC   | Install the Device Plugins required by the environment                                            |

Review the [Sysbox limitations](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/limitations.md)
before selecting it for environments that need devices or additional Linux
capabilities.

Label Kata and Sysbox RuntimeClasses so Manager can match them to an isolation
mode:

```sh
kubectl label runtimeclass kata-qemu-runtime-rs \
  codespace.gitea.dev/isolation=kata --overwrite
kubectl label runtimeclass sysbox-runc \
  codespace.gitea.dev/isolation=sysbox --overwrite
kubectl get runtimeclass -L codespace.gitea.dev/isolation
kubectl get storageclass
```

Native environments use the node's default CRI runtime. Install the required
Device Plugin and enable CDI when the device integration requires it. Confirm
that each extended resource appears in node allocatable capacity:

```sh
kubectl get nodes \
  -o custom-columns='NAME:.metadata.name,ALLOCATABLE:.status.allocatable'
```

Select the resource and CDI settings in the environment template. Validate
nested containers, persistent storage, and device access before making the
template available to users.

### Verify

- A workspace Pod starts with the selected isolation mode and StorageClass.
- Workspace data remains available after a normal stop and start.
- Dev Container builds can run nested containers with Kata or Sysbox.
- Native environments receive the selected extended resources, CDI devices,
  capabilities, and mounts.

## Install

Store the administration token in a Secret so Helm values and release history
do not contain it.

```sh
kubectl create namespace codespace-system
kubectl -n codespace-system create secret generic codespace-admin \
  --from-literal=token="$(openssl rand -hex 32)"

helm upgrade --install codespace \
  oci://ghcr.io/explodingdragon/charts/gitea-codespace \
  --devel \
  --namespace codespace-system \
  --set admin.tokenSecret.name=codespace-admin \
  --wait \
  --timeout 5m
```

This installs the latest `main` prerelease. Published prerelease versions use
the form `0.1.0-main.<run>`; replace `--devel` with
`--version 0.1.0-main.<run>` when the installation must select one exact
chart. The source chart defaults to the `main` image tag with no digest for
local development, while each published chart contains the digest of the
platform image built for that chart.

## Configure Web IDE storage and image access

`webIDE.programVolumeSize` sets the size limit of the disk-backed `emptyDir`
that holds the selected Web IDE program. It does not reserve disk space;
nodes also need enough free storage for the unpacked bundle. Increase the limit
when the bundle exceeds the default limit. Configure `image.pullSecrets` when
platform or provider images are stored in a private registry; the same
credentials are available to workspace Pods. See the
[Web IDE provider guide](../../providers/README.md) for image selection and
editor configuration.

## Connect Gitea

The administration Service is a ClusterIP by default. Access it from an
operator workstation with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Open <http://127.0.0.1:18080> and sign in with the administration token. Follow
the project [connection guide](../../README.md#connect-gitea) to create the
platform resources and register Gitea.

For a stable administration endpoint, configure `admin.ingress`. When Network
Policy is enabled, add the ingress controller networks to
`networkPolicy.adminIngressCIDRs`.

## Publish workspace access

Each Gateway has a same-name ClusterIP Service. Port `80` carries HTTP and
WebSocket traffic; port `22` carries SSH and SFTP. Publish HTTP with an Ingress
and SSH with a LoadBalancer Service, or route the ClusterIP Service through
existing cluster infrastructure. Configure public DNS and TLS at that boundary.

### Verify

- The published HTTP address supports browser navigation and WebSocket
  upgrades through the same Gateway Service.
- The published SSH address supports both SSH and SFTP on the configured host
  and port.
- Gateway Services remain reachable inside the cluster when public publishing
  is managed by separate infrastructure.

## Operations

Back up `Secret/<Helm fullname>-internal-ca` because running workspaces and
components trust this identity. Enable Kubernetes Secret encryption at rest.
Add a private Gitea CA to the site configuration when Gitea does not use a
publicly trusted certificate.

Each Runtime combines the system roots, the internal service root, the site
CA, and the optional proxy CA before starting its Docker daemon. Git, image
pulls, OCI Feature package downloads, and the running Dev Container use this
trust set. Dockerfile `RUN` commands and Feature installation scripts execute
inside the build image and use that image's own certificate configuration.
Component mTLS identities remain in separate Secrets and are not exposed to
the Dev Container.

Configure Gateway trusted proxy CIDRs when a reverse proxy connects directly to
Gateway. Allow configured outbound proxies through the site NetworkPolicy.

Cache is cluster-internal and uses Manager's internal CA. Use a persistent
volume for the local backend or shared object storage for the S3 backend.

Cache cleanup pauses new use of that Cache while allowing active image pushes
to finish. New workspaces use another available Cache or continue without cache
acceleration, so cleanup does not change workspace lifecycle state.

The StorageClass of a Cache PVC is selected when the PVC is created and then
becomes part of the running storage configuration. The administration page
shows the actual class for an existing PVC; changing it requires creating a new
Cache and rebinding the site because Kubernetes does not migrate an existing
PVC between StorageClasses. Cache data is reproducible, so this operation does
not require a data migration workflow.

### Verify

- Runtime Pods can reach the Cache Service through its `443/https` endpoint.
- A site with a private CA can clone repositories and pull Dev Container or
  OCI Feature images from that Gitea installation.
- Cache cleanup completes without interrupting active image pushes or workspace
  lifecycle operations.
- Editing an existing Cache shows its actual StorageClass and does not report a
  changed class as successfully applied.

## Enable Kata USB/IP

USB/IP is discovered from the running Kata guest rather than configured on an
environment template. This keeps the template tied to its isolation boundary:
an ordinary Kata kernel starts normally, while a kernel with an active VHCI
controller also exposes the USB/IP workflow.

Build or select a Kata guest kernel with `CONFIG_DEVTMPFS`, `CONFIG_USB`,
`CONFIG_USBIP_CORE`, `CONFIG_USBIP_VHCI_HCD`, and the drivers for the intended
devices. For example, CDC ACM serial devices require `CONFIG_USB_ACM`. Building
these options into the kernel makes them available without maintaining a guest
module installation. When either USB/IP option is a module, load it during guest
startup so `/sys/devices/platform/vhci_hcd.*` exists before the Runtime Agent
starts. The platform image supplies the `usbip` client, `lsusb`, and the USB
vendor/product database; the guest root filesystem supplies the kernel's
startup environment.

The supplied [kernel configuration](../../tests/e2e/kata-usbip.config) also
includes these device drivers:

| Devices | Guest support |
| --- | --- |
| USB disks | SCSI disk, USB Storage/UAS, MBR/GPT, ext4, FAT, exFAT, NTFS3 |
| Serial adapters | CDC ACM, FTDI, CP210x, CH341, PL2303 |
| HID debug probes | USB HID, generic HID, HIDRAW |
| I2C/GPIO bridges | CP2112, MCP2221, MCP2200 with their supported interfaces |
| SPI bridges | CH341 controller and spidev; child device binding depends on the hardware |
| Test instruments | USBTMC |

USB networking and CAN are disabled in this configuration so imported devices
do not add interfaces to the Pod's network namespace. The existing virtio,
veth, bridge, and IP networking remains available to the Runtime and Docker.
Use the [kernel build guide](../../tests/e2e/README.md#prepare-kata-usbip-tests)
to produce and validate the kernel for the selected Kata runtime.

After starting a Kata Codespace, verify the Runtime Pod before publishing the
workflow to users:

```sh
kubectl exec -n '<site-namespace>' '<runtime-pod>' -c runtime -- \
  sh -c 'test -d /sys/devices/platform/vhci_hcd.0 && command -v usbip'
```

The primary Dev Container sees the guest's device nodes under `/dev`. Its image
starts udev or eudev and supplies device rules, groups, and non-root permissions.
The platform allows raw USB, USB serial, CDC ACM, I2C, SPI, USB miscellaneous
devices (including USBTMC), and the guest's registered HIDRAW and GPIO character
device classes. HIDRAW and GPIO major numbers are discovered at container
creation because the kernel allocates them dynamically. Load those drivers
before creating the Dev Container; the supplied configuration builds them in.
Existing containers retain their creation-time device rules. The `NET_ADMIN`
capability supports eudev's netlink events. Block-device access and
filesystem mounting use the permissions explicitly declared by the development
configuration. Kernel support, device access rules, and userspace permissions
must all be verified together.

The isolation boundary is the Kata virtual machine assigned to one Codespace;
processes in that workspace share its attached hardware. Configure the Kata
handler with `privileged_without_host_devices=true` so privileged workloads
receive guest capabilities without implicitly receiving Kubernetes node
devices. Review explicit `privileged`, `devices`, `capAdd`, and network-sharing
settings in development configurations because they can broaden access within
the guest. The default USB device-class rules are not per-device restrictions.
See the [Kata USB design](https://github.com/ExplodingDragon/gitea-dev/blob/main/src/codespace/runtime-platform.md#kata-usb-%E8%AE%BE%E5%A4%87)
for the mount and forwarding implementation.

SSH authenticates the connection; the device server and guest drivers still
need their own security review. Protect the workstation's USB/IP endpoint from
unintended clients.

Validate the complete path with a disposable exported device before production
use, following the [USB connection guide](../../docs/usbip.md). Verify device
access as the configured non-root user. The kernel build guide above also
covers automated USB/IP validation on a disposable worker.
Stop hooks should tolerate a device already being offline, because the Gateway
can revoke access before the hook runs.

### Verify

- USB/IP-capable environments expose the command and IDE controls; ordinary
  Kata environments start normally with those optional controls hidden.
- A compatible guest lists, attaches, uses, and detaches a remote device
  without restarting the Dev Container.
- udev or eudev rules in the development image receive hotplug events and set
  the expected non-root ownership and permissions.
- Device access and ordinary terminal use remain functional after a stop/start
  cycle and a new device attachment.
- The selected kernel provides the intended drivers and filesystems; test
  hardware is usable with the declared container permissions. A storage test
  uses a disposable disk and explicitly granted block-device and mount access.
- HIDRAW and GPIO access rules use the running guest's major numbers. The
  supplied kernel keeps USB networking and CAN disabled while normal Pod and
  Docker networking works.
- Multiple named USB/IP sources coexist with ordinary Dev Container reverse
  forwards. Source names are unique within one Codespace. Closing a source
  disconnects only its devices; closing an SSH session releases its forwards.
  Stopping the Codespace or replacing its Runtime releases all bridge connections.

## Resource monitoring

Manager prefers the cluster Metrics API for Runtime CPU and memory usage,
with Agent cgroup measurements as a fallback. Install Metrics Server if the
cluster does not already provide `metrics.k8s.io`. Limits come from the actual
Runtime Pod configuration; an unset limit is displayed as unlimited.

Filesystem usage combines Runtime observations with kubelet volume statistics
when the cluster exposes them. Kata block volumes use the mounted filesystem
observation. The displayed total is the observed filesystem capacity; usage is
that capacity minus the smallest available-space observation. This includes
space reserved by the filesystem rather than only workspace file sizes. The
PVC request is not a measurement of free space, and shared filesystem capacity
is not an exclusive workspace quota.

The chart grants `list` on `metrics.k8s.io/pods` for CPU and memory statistics
and `get` on `nodes/proxy` for kubelet filesystem statistics. Kubernetes grants
the latter permission for the complete node proxy. Operators managing RBAC
separately can omit it and use Runtime filesystem observations instead. Missing
monitoring APIs or permissions affect displayed statistics, not workspace
lifecycle.

### Verify

- CPU and memory usage match the Runtime container in `kubectl top pod
  --containers`, allowing for different sample times.
- Filesystem usage is derived from observed capacity and available space;
  a valid zero available-space observation is shown as full.
- A metrics outage falls back to Runtime observations without changing the
  workspace lifecycle.

## Upgrade

Helm does not upgrade CRDs placed in the chart's `crds` directory. Apply the
CRDs from the target chart version before upgrading Manager so the API server
accepts the new resource shape:

```sh
CHART=oci://ghcr.io/explodingdragon/charts/gitea-codespace
VERSION='<chart-version>'

helm show crds "$CHART" --version "$VERSION" |
  kubectl apply --server-side --force-conflicts \
    --field-manager=gitea-codespace-crds -f -

helm upgrade --install codespace "$CHART" \
  --version "$VERSION" \
  --namespace codespace-system \
  --set admin.tokenSecret.name=codespace-admin \
  --wait
```

Using the same version keeps CRDs, Manager, and the platform image aligned.
`--force-conflicts` is required when an earlier Helm release owns the CRD version
field; it transfers that field to the dedicated CRD manager used by this command.

## Chart values

[`values.yaml`](values.yaml) lists the defaults and
[`values.schema.json`](values.schema.json) defines the accepted values. Use a
values file for installation-specific settings so upgrades remain repeatable.
