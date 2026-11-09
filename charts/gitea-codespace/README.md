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

This installs the latest `main` prerelease. For a release, replace `--devel`
with `--version <chart-version>`. Published charts select the matching platform
image by digest.

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

## Operations

Back up `Secret/<Helm fullname>-internal-ca` because running workspaces and
components trust this identity. Enable Kubernetes Secret encryption at rest.
Add a private Gitea CA to the site configuration when Gitea does not use a
publicly trusted certificate.

Configure Gateway trusted proxy CIDRs when a reverse proxy connects directly to
Gateway. Allow configured outbound proxies through the site NetworkPolicy.

Cache is cluster-internal and uses Manager's internal CA. Use a persistent
volume for the local backend or shared object storage for the S3 backend.

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
