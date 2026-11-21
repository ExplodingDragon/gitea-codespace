# Gitea Codespace Helm chart

This chart installs Codespace Manager and its administration service. Use the
administration page to connect Gitea and configure workspace environments,
Gateway, and Cache components.

## Requirements

- Kubernetes 1.36 or later
- A CRI runtime supporting image volumes and their `subPath` mounts, such as
  containerd 2.2 or later
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

Select the resource and its propagation settings in the environment template.
Suggested NVIDIA, Intel i915/xe, and AMD settings are editable starting points;
validate them against the installed plugin and Runtime image before publishing
the template. Filling suggestions preserves existing CDI or manual device
mappings. Kubernetes reserves the resource; the Agent prepares the devices
for the primary Dev Container on each create or resume.

| Setting | Source and behavior |
| --- | --- |
| CDI `selection: environment` | `deviceIDEnvironment` names a Runtime variable containing comma-separated IDs or fully qualified names for `kind`. NVIDIA envvar deployments can use `NVIDIA_VISIBLE_DEVICES` with `nvidia.com/gpu`. |
| CDI `selection: deviceNodes` | `deviceNodePaths` identifies Runtime device nodes, for example `/dev/dri/card*` with `intel.cdi.k8s.io/gpu`. Each visible identifying node must match one CDI device. Ambiguous aliases or shared control nodes require an allocation environment or manual mappings. |
| Manual devices | Map a Runtime device or directory to the Dev Container. Directories expand to their character and block devices. |
| Manual mounts | Choose `sourceScope: runtime` for paths already in the Runtime, or `node` for an explicit administrator-authorized hostPath. Node paths are staged separately so they do not replace Runtime system directories. |
| Forwarded environment | Read the named variables from the current Runtime Pod, including after resource reallocation. |
| Additional groups | Numeric GIDs grant device access to the main process and remote user commands. Use the actual device GIDs; group names and numbers can differ between images and nodes. Set `remoteUser` to an existing username or UID, because Docker exec omits supplementary groups when an explicit `user:group` is given. |

CDI definitions describe node-wide devices; selection identifies the current
Pod's allocation. A shared allocation can use fewer physical CDI devices than
the requested resource count. The Agent uses the device and mount paths already
exposed in the Runtime, so verify those paths and any required CDI hook
executables and dependencies in the Runtime image. Manual mappings use those
Runtime paths directly and can be configured independently of node CDI definitions.

Native Runtime Pods include `SYS_PTRACE` so Docker and Agent can access the
network namespace of a container whose main process is non-root. This applies
within the Runtime's private process namespace; Pod admission policies must
permit the required capabilities shown in the template editor.

### Verify

- A workspace Pod starts with the selected isolation mode and StorageClass.
- Workspace data remains available after a normal stop and start.
- Dev Container builds can run nested containers with Kata or Sysbox.
- Native environments receive the selected extended resources, CDI devices,
  capabilities, and mounts.
- Device access works as the configured remote user through SSH, Web IDE, and
  lifecycle commands, including after a stop and start on a different allocation.
- Containers with a non-root main process start and support TCP forwarding.

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

## Configure event limits

`events.maxPerSecond` sets the maximum accepted workspace events per Codespace
in any one-second interval. It defaults to `5` and must be a positive integer.
Ordinary platform and build logs keep their existing limits.

Changing this value rolls the Manager deployment and applies to existing and
new Codespaces without rebuilding their Dev Containers. Each instance shares
one allowance across event types and callers, so launching more processes does
not increase its quota. Counters are in memory and reset on leader replacement.
See [Workspace events](../../docs/events.md) for the command and delivery limits.

## Configure Web IDE storage and image access

Web IDE programs are mounted read-only from the selected OCI image. Tags use
`pullPolicy: Always`; explicit digests use `IfNotPresent`. New or replacement
Pods resolve tags again, while running Pods retain their mounted content.
Nodes need space for pulled and unpacked images;
kubelet and the container runtime manage their cache and garbage collection.
Provider settings and installed extensions remain on the workspace volume so
they survive Pod replacement.

Configure `image.pullSecrets` when platform or provider images are stored in a
private registry. Manager uses these credentials to resolve provider metadata,
and the same credentials are available to workspace Pods. Registry connectivity
and certificate trust must work at both stages:

| Stage | Required access |
| --- | --- |
| Template configuration | Manager resolves the image and reads its metadata using the Manager process's network, proxy, and certificate trust. |
| Pod startup | The node's container runtime pulls and mounts the image using its own registry, proxy, and certificate configuration. |

Pull Secrets supply authentication, not CA trust or proxy configuration. The
site CA and Runtime trust bundle apply to Gitea connections and workspace
downloads; they do not configure Manager's provider registry client or the
node's container runtime. See the
[Web IDE provider guide](../../providers/README.md) for image selection and
editor configuration.

### Verify

- Resolve the provider image in an environment template, then create a
  workspace on each node pool to check image pulls and the read-only bundle mount.
- Open the editor as the Dev Container remote user, change a setting, and stop
  and resume the workspace to check that the setting persists.

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
Configure Gateway trusted proxy CIDRs when a reverse proxy connects directly to
Gateway.

Workspace HTTP requests renew the browser session; active WebSocket and SSE
connections keep it valid, with the idle timeout starting when the last request
ends. Authenticated application ports retain a fixed session lifetime and their
own idle timeout; their traffic does not renew the Workspace session. Reopen an
expired session from Gitea so Gateway can authenticate it again.

### Verify

- The published HTTP address supports browser navigation and WebSocket
  upgrades through the same Gateway Service.
- The published SSH address supports both SSH and SFTP on the configured host
  and port.
- Gateway Services remain reachable inside the cluster when public publishing
  is managed by separate infrastructure.
- An active Workspace remains accessible across its ticket lifetime; ordinary
  application port traffic does not extend an idle Workspace session.

## Configure network access

Configure Runtime network access in **Gitea sites > Network access** before
creating workspaces. These rules also cover the nested Dev Container's traffic.
Internet access and communication between Codespaces in the same site are
separate switches, both disabled by default. Internet access allows outbound
TCP/UDP to public addresses; add internal CIDRs to exclude organization networks
that use public addresses. Explicit destinations grant a CIDR access through the
selected TCP/UDP protocols and individual ports, inclusive port ranges, or all
ports. These grants also apply when Internet access is disabled, so they can
authorize internal services and outbound proxies.

Allow the actual Gitea HTTP or SSH clone destination and the registries and
services needed by builds. A private Gitea address needs an explicit grant even
when Internet access is enabled. CIDR rules apply to destination IP addresses,
so account for the addresses a hostname resolves to. Successful Manager
registration verifies its control connection, not the Runtime's outbound path.

Gateway, Manager, Cache, and cluster DNS retain their required connections.
The same-site switch permits bidirectional TCP/UDP between Runtime Pods,
including different users' Codespaces. Changes apply to existing and new Pods
through the cluster's NetworkPolicy controller. The network plugin must enforce
`endPort` for port ranges; existing connections may survive a policy update.
NetworkPolicy does not isolate a Pod from its resident node, and a permitted
proxy or Cache makes its own upstream connections. Keep their access rules
appropriate for the services they expose.

### Verify

- A workspace can clone from its configured Gitea destination and reach the
  required build services through the selected direct or proxy path.
- TCP/UDP connections follow the configured CIDRs, protocols, and port ranges.
- Direct communication between the site's Runtime Pods follows the same-site
  switch, and Runtime Pods in other sites remain isolated.
- Gateway workspace access and required platform connections continue to work.

## Operations

### Certificate trust

Back up `Secret/<Helm fullname>-internal-ca` because running workspaces and
components trust this identity. Enable Kubernetes Secret encryption at rest.
Add a private Gitea CA to the site configuration when Gitea does not use a
publicly trusted certificate.

Each Runtime combines the system roots, the internal service root, the site
CA, and the optional proxy CA before starting its Docker daemon and build
clients. Git, registry token requests, HTTPS and OCI Feature downloads, and
`initializeCommand` use this trust set.

The Dev Container image must include its distribution's `ca-certificates`
bundle. Before starting or resuming the primary container, Agent adds platform
certificates to that container's own system bundle, preserving the image's
public and enterprise roots. Each controlled start or resume combines the current
site CA with the proxy CA saved in the instance's configuration snapshot;
editing a template's proxy CA affects subsequently created instances.
Shells, IDEs, and systemd services use the prepared trust from their first process
start. Compose companion services retain their own trust configuration.
Dockerfile `RUN` commands and Feature installation scripts
execute inside the build image and use that image's own certificate configuration.
Component mTLS identities remain in separate Secrets and are not exposed to
the Dev Container.

#### Verify

- Create a workspace using the site's private CA and check repository cloning,
  image builds, and HTTPS or OCI Feature downloads.
- Check HTTPS access to the required public and enterprise services from the
  primary container, then repeat after stop and resume to verify its trust bundle.

### Cache maintenance

Cache is cluster-internal and uses Manager's internal CA. Use a persistent
volume for the local backend or shared object storage for the S3 backend.

The maximum cache size covers committed blobs in both build and mirror storage.
It is a soft limit checked during maintenance; the free-space reserve checks
actual filesystem space, including temporary uploads. Retention age is measured
from repository updates, not reads. Under disk pressure, already cached digests
remain readable, while mirror tag refreshes and cache misses fall back upstream.

Cleanup checks for reclaimable data before pausing new use of that Cache.
Existing uploads can continue while their credentials remain valid; cleanup
starts after that window and has a limited duration. When a Cache is unavailable,
including when its access credentials cannot be prepared safely, new workspaces
use another available Cache or continue without cache acceleration. Direct image
pulls and OCI Feature downloads through a mirror fall back to the original
registry after two minutes without progress; ongoing transfers can take longer.
Feature image cache transfers use the same idle deadline, and cache publishing
failures preserve the built image.
Dockerfile image references are resolved by BuildKit; build-layer cache reuse
and direct image mirroring are separate paths. Direct registry access still
follows the site's outbound network policy and the registry's authentication and
certificate requirements.

The StorageClass of a Cache PVC is selected when the PVC is created and then
becomes part of the running storage configuration. The administration page
shows the actual class for an existing PVC; changing it requires creating a new
Cache and rebinding the site because Kubernetes does not migrate an existing
PVC between StorageClasses. Cache data is reproducible, so this operation does
not require a data migration workflow.

Check connectivity from a Runtime Pod to the Cache Service's `443/https`
endpoint when diagnosing cache access.

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

Stop hooks should tolerate a device already being offline, because the Gateway
can revoke access before the hook runs.

### Verify

Before production use, follow the [USB connection guide](../../docs/usbip.md)
with disposable hardware representative of the intended devices:

- List, attach, use, and detach a disposable device as the configured non-root
  user. Check the resulting device nodes and udev permissions; a storage test
  also needs explicitly granted block-device and mount access.
- Stop and resume the workspace, reconnect the USB source, and repeat the
  device operation while checking ordinary terminal and network access.
- If using several device servers, connect two named sources and confirm that
  closing one leaves the other's devices usable.

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

- Compare CPU and memory usage with the Runtime container in `kubectl top pod
  --containers`, allowing for different sample times.
- Compare the displayed filesystem capacity and available space with the
  mounted workspace filesystem, accounting for any shared storage.

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
