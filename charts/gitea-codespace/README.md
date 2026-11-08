# Gitea Codespace Helm chart

This chart installs Codespace Manager. Manager then creates Gateway, Cache, and
workspace resources configured through the administration page.

## Requirements

- Kubernetes 1.31 or later
- Helm 4.3 or later
- Gitea with Codespace support
- A compatible StorageClass
- A verified Kata or Sysbox RuntimeClass when that runtime is selected

Test the runtime and StorageClass together on every workspace node pool. Each
workspace runs Docker inside its Pod, so ordinary Pod startup alone is not a
sufficient runtime check.

## Prepare a runtime

| Runtime | Isolation | Workspace volume | Installation |
| --- | --- | --- | --- |
| Kata Containers | Virtual machine | Raw block PVC | [Kata guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md) |
| Sysbox | User namespace | Filesystem PVC | [Sysbox guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md) |
| Native | Node kernel | Filesystem PVC | Default CRI runtime and vendor Device Plugins |

Review the [Sysbox limitations](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/limitations.md)
before using devices or elevated privileges with that runtime.

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

Native environments use the node's default CRI runtime. Install each vendor
Device Plugin before creating a Native template and confirm that its extended
resource appears in node allocatable capacity:

```sh
kubectl get nodes \
  -o custom-columns='NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu'
```

CDI propagation also requires CDI-enabled CRI allocation, matching node CDI
specifications, and a Device Plugin environment variable containing the
allocated IDs. For NVIDIA, configure the plugin to provide both forms:

```yaml
flags:
  plugin:
    deviceListStrategy: envvar,cdi-cri
```

Select the extended resource, CDI kind, and allocation environment in the
environment template. Enable **Satisfies Dev Container GPU requirements** when
that allocation should fulfill repository GPU requests. Advanced host access
can forward an allocated device path, mount a driver directory, or pass a
Device Plugin environment variable. Site device quotas limit the total extended
resources that concurrent workspaces may reserve.

Before publishing a template, verify that a workspace can start nested Docker,
run a child container, access its allocated device, and retain that access after
a stop and resume. This checks the runtime, storage, Device Plugin, and CDI path
as one deployable combination.

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

This selects the latest `main` prerelease. For a release, replace `--devel` with
`--version <chart-version>`. Published charts select their matching platform
image by digest. Configure `image.pullSecrets` when the image registry requires
authentication.

## Configure Manager

The administration Service is a ClusterIP by default. Access it from an
operator workstation with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Open <http://127.0.0.1:18080> and sign in with the administration token. Then:

1. create a Gateway and any Cache components;
2. create an environment template using the verified runtime and storage;
3. create a registration token in Gitea's Codespace Managers page;
4. register Gitea and assign the template and components;
5. enable the site after its readiness checks pass.

Configure `admin.ingress` when the administration page needs a stable endpoint.
Add the ingress controller networks to `networkPolicy.adminIngressCIDRs`.

## Publish workspace access

Each Gateway has a same-name ClusterIP Service: port `80` carries HTTP and
WebSocket traffic, while port `22` carries SSH and SFTP. Manager can create an
HTTP Ingress and an SSH LoadBalancer Service, or existing cluster infrastructure
can publish the ClusterIP Service. DNS and external TLS remain cluster
responsibilities.

## Security and network access

Back up `Secret/<Helm fullname>-internal-ca` because existing workloads trust
it. Enable Kubernetes Secret encryption at rest. Add a private Gitea CA to the
site configuration when required.

Configure Gateway trusted proxy CIDRs for direct proxies. Add environment proxy
CIDRs and ports to each affected site so its NetworkPolicy permits the traffic.
Cache proxy settings remain separate because registry traffic can use another
network path.

Cache Services use Manager's internal CA and remain inside the cluster on port
`443`. The local backend uses a persistent volume; the S3 backend uses shared
object storage.

## Upgrade

Helm installs CRDs from a chart but does not upgrade them. Apply the CRDs from
the exact chart version before upgrading Manager so the API server accepts the
new resource shape:

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

The shared version keeps CRDs, Manager, and the platform image aligned.
`--force-conflicts` transfers CRD ownership from an earlier Helm or kubectl
field manager to the dedicated CRD manager.

## Chart values

[`values.yaml`](values.yaml) lists the defaults and
[`values.schema.json`](values.schema.json) defines the accepted values. Use a
values file for installation-specific settings so upgrades remain repeatable.
