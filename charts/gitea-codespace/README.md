# Gitea Codespace Helm chart

This chart installs Codespace Manager and the cluster resources it owns.
Manager creates Gateway, Cache, and workspace workloads from the administration
page after installation.

## Requirements

- Kubernetes 1.31 or later
- Helm 4.3 or later
- Gitea with Codespace support
- A Kata Containers or Sysbox RuntimeClass verified on the workspace nodes
- A compatible StorageClass

Runtime support depends on the worker operating system, architecture, container
runtime, and storage driver. Test the RuntimeClass and StorageClass together on
the intended worker pool before assigning them to users.

## Prepare the workspace runtime

Each workspace runs Docker inside its Pod, so the selected RuntimeClass must
support nested containers. Runtime and storage behavior are coupled; verify
them together on the worker pool that will host workspaces.

| Runtime         | Isolation       | Workspace volume | Installation                                                                                    |
| --------------- | --------------- | ---------------- | ----------------------------------------------------------------------------------------------- |
| Kata Containers | Virtual machine | Raw block PVC    | [Kata guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md) |
| Sysbox          | User namespace  | Filesystem PVC   | [Sysbox guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md)   |

Install a pinned runtime release, verify nested Docker and storage on the target
workers, then label the RuntimeClass so Manager can offer it:

```sh
kubectl label runtimeclass kata-qemu-runtime-rs \
  codespace.gitea.dev/isolation=kata \
  --overwrite
kubectl label runtimeclass sysbox-runc \
  codespace.gitea.dev/isolation=sysbox \
  --overwrite
kubectl get runtimeclass -L codespace.gitea.dev/isolation
kubectl get storageclass
```

Review the [Sysbox limitations](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/limitations.md)
before assigning environments that use devices or elevated privileges.

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

## Configure the installation

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

To publish the administration page, configure `admin.ingress` and add the
ingress controller networks to `networkPolicy.adminIngressCIDRs`.

## Publish workspace access

Each Gateway has a same-name ClusterIP Service. Port `80` carries HTTP and
WebSocket traffic; port `22` carries SSH and SFTP. The component configuration
can create an HTTP Ingress and an SSH LoadBalancer Service, or an operator can
publish the ClusterIP Service through existing infrastructure. Public DNS, TLS,
and load-balancer policy remain cluster responsibilities.

## Security and network access

Back up `Secret/<Helm fullname>-internal-ca` because existing workloads trust
it, and enable Kubernetes Secret encryption at rest. Add a private Gitea CA to
the site configuration so Manager and workspaces trust it.

Configure Gateway trusted proxy CIDRs for direct proxies. Add environment proxy
CIDRs and ports to each affected site so its NetworkPolicy permits the traffic.
Cache proxy settings remain separate because registry traffic can use another
network path.

Cache Services use Manager's internal CA and remain inside the cluster on port
`443`. Use the local backend for a single persistent Cache or S3 for shared
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
