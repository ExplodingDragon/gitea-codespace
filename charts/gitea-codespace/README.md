# Gitea Codespace Helm chart

This chart installs Codespace Manager. After installation, use the
administration page to connect Gitea and manage workspace environments, Gateway,
and Cache components.

## Requirements

- Kubernetes 1.31 or later
- Helm 4.3 or later
- Gitea with Codespace support
- A compatible StorageClass
- A verified Kata or Sysbox RuntimeClass when using that runtime

Test the runtime and StorageClass together on every workspace node pool. Each
workspace runs Docker inside its Pod, so ordinary Pod startup alone is not a
sufficient runtime check.

## Choose a workspace runtime

| Runtime         | Isolation       | Workspace volume | Preparation                                                                                                                   |
| --------------- | --------------- | ---------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Kata Containers | Virtual machine | Raw block PVC    | [Install Kata](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md)                              |
| Sysbox          | User namespace  | Filesystem PVC   | [Install Sysbox](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md)                               |
| Native          | Node kernel     | Filesystem PVC   | Use the default CRI runtime and install the Device Plugins required by the environment                                       |

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

Native environments use the node's default CRI runtime. Install the vendor
Device Plugin before creating the environment, then confirm that its extended
resource appears in node allocatable capacity. For example:

```sh
kubectl get nodes \
  -o custom-columns='NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu'
```

CDI devices require CDI support in the node runtime and Device Plugin. For
NVIDIA, enable both the environment-variable and CDI allocation strategies:

```yaml
flags:
  plugin:
    deviceListStrategy: envvar,cdi-cri
```

Select the extended resource and CDI settings in the environment template. Site
device quotas can limit how many extended resources concurrent workspaces
reserve. Before publishing the template, verify nested Docker, a child
container, device access, and device access after stop and resume. This validates
the runtime, storage, Device Plugin, and CDI configuration together.

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

This installs the latest `main` prerelease. To install a release, replace
`--devel` with `--version <chart-version>`. Published charts select the matching
platform image by digest. Configure `image.pullSecrets` for a private registry.

## Complete setup

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

For a stable administration endpoint, configure `admin.ingress` and allow the
ingress controller networks through `networkPolicy.adminIngressCIDRs`.

## Publish workspace access

Each Gateway has a same-name ClusterIP Service. Port `80` carries HTTP and
WebSocket traffic; port `22` carries SSH and SFTP. Manager can create an HTTP
Ingress and an SSH LoadBalancer Service, or existing cluster infrastructure can
publish the Service. Configure public DNS and TLS in the cluster infrastructure.

## Operate securely

Back up `Secret/<Helm fullname>-internal-ca` because existing workloads trust
this internal identity. Enable Kubernetes Secret encryption at rest. Add a
private Gitea CA to the site configuration when Gitea does not use a publicly
trusted certificate.

Configure Gateway trusted proxy CIDRs when a reverse proxy connects directly to
Gateway. When a site uses an outbound proxy, allow that proxy's CIDRs and ports
through the site NetworkPolicy. Cache has separate proxy settings because image
registry traffic may use a different network path.

Cache remains inside the cluster on port `443` and uses Manager's internal CA.
Choose a persistent volume for the local backend or shared object storage for
the S3 backend.

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

Using the same version keeps CRDs, Manager, and the platform image aligned.
`--force-conflicts` is required when an earlier Helm release owns the CRD version
field; it transfers that field to the dedicated CRD manager used by this command.

## Chart values

[`values.yaml`](values.yaml) lists the defaults and
[`values.schema.json`](values.schema.json) defines the accepted values. Use a
values file for installation-specific settings so upgrades remain repeatable.
