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

Configure `image.pullSecrets` when the platform or Web IDE images are stored in
a private registry. The same credentials are made available to workspace Pods.

## Configure Web IDE access

Environment templates contain one Web IDE image field. Leaving it empty
disables browser access. The administration page suggests the official VS Code
and DeepSeek Harness images and also accepts compatible third-party images.
Manager validates the image options and stores its immutable digest when the
environment is saved. Existing environments therefore keep the selected editor
version when a mutable tag is updated.

`webIDE.programVolumeSize` reserves temporary Pod storage for the selected
editor program. Increase it when a provider image needs more unpacked space.
See the [Web IDE provider guide](../../providers/README.md) when using a custom
provider.

### Verify

- Leaving the provider empty creates a codespace with SSH and forwarded ports
  but no browser workspace endpoint.
- Resolving a provider displays its identity and options, and saving the
  environment records an immutable image digest.
- A codespace starts the provider on either supported node architecture with
  its persistent configuration intact.

## Connect Gitea

The administration Service is a ClusterIP by default. Access it from an
operator workstation with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Open <http://127.0.0.1:18080>, sign in with the administration token, and then:

1. create a Gateway and any Cache components;
2. create an environment template using the verified runtime and storage;
3. create a registration token in Gitea's Codespace Managers page;
4. register Gitea and assign the template and components;
5. enable the site after its readiness checks pass.

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

Configure Gateway trusted proxy CIDRs when a reverse proxy connects directly to
Gateway. Allow configured outbound proxies through the site NetworkPolicy.

Cache is cluster-internal and uses Manager's internal CA. Use a persistent
volume for the local backend or shared object storage for the S3 backend.

Cache cleanup is coordinated with the existing Cache GC Lease. During cleanup,
new cache credentials are temporarily withheld while credentials already issued
remain valid long enough to finish a multi-request image push. New workspaces
use another Cache when one is available; when no Cache is available, the
workspace can continue without cache acceleration. This keeps cleanup from
removing a layer between upload and manifest submission without changing
workspace lifecycle state. A Cache control-stream reconnect or Manager restart
continues the existing cleanup deadline instead of extending the lifetime of
already issued credentials.

The StorageClass of a Cache PVC is selected when the PVC is created and then
becomes part of the running storage configuration. The administration page
shows the actual class for an existing PVC; changing it requires creating a new
Cache and rebinding the site because Kubernetes does not migrate an existing
PVC between StorageClasses. Cache data is reproducible, so this operation does
not require a data migration workflow.

### Verify

- Runtime Pods can reach the Cache Service through its `443/https` endpoint.
- An image push that overlaps the beginning of Cache cleanup can finish its
  blob and manifest requests before cleanup starts.
- Editing an existing Cache shows its actual StorageClass and does not report a
  changed class as successfully applied.

## Resource monitoring

Manager prefers the cluster Metrics API for Runtime CPU and memory usage,
with Agent cgroup measurements as a fallback. Install Metrics Server if the
cluster does not already provide `metrics.k8s.io`. Limits come from the actual
Runtime Pod configuration; an unset limit is displayed as unlimited.

CPU and memory are collected every 30 seconds. Filesystem volume statistics
are collected from kubelet Summary once per relevant node every 60 seconds.
Manager combines valid Agent and kubelet available-space measurements by
taking the smaller value. Kata block volumes use the Agent's mounted
filesystem. PVC requested capacity is not a free-space measurement, and a
shared filesystem's free space is not an exclusive workspace quota.

The chart grants `list` on `metrics.k8s.io/pods` and `get` on `nodes/proxy`.
The latter lets Manager reach kubelet Summary through the Kubernetes API;
Kubernetes authorizes the whole node proxy rather than only the Summary path,
so this permission grants broader kubelet access. Operators managing RBAC
separately can omit it and use Agent filesystem measurements instead. Missing
monitoring APIs or permissions affect statistics, not workspace lifecycle.

### Verify

- CPU and memory usage match the Runtime container in `kubectl top pod
  --containers`, allowing for different sample times.
- Filesystem statistics report available space rather than the PVC request;
  a valid zero available-space observation is shown as full.
- A temporary metrics outage retains fresh observations and then falls back
  to Agent measurements; observations expire instead of becoming zero usage.

## Upgrade

Helm does not upgrade CRDs placed in the chart's `crds` directory. Apply the
CRDs from the target chart version before upgrading Manager so the API server
accepts the new resource shape:

```sh
CHART=oci://ghcr.io/explodingdragon/charts/gitea-codespace
VERSION=<chart-version>

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
