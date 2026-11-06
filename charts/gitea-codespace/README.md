# Gitea Codespace Helm chart

This chart installs the Codespace Manager, CRDs, RBAC, administration Service,
and NetworkPolicy. Manager then creates Gateway, Cache, and Runtime workloads
from the resources configured in its administration page.

## Requirements

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- a Gitea installation with Codespace support;
- an administrator token stored in a Kubernetes Secret;
- a verified Kata Containers or Sysbox RuntimeClass; and
- a StorageClass compatible with the selected runtime.

Runtime support depends on the worker operating system, architecture, CRI, and
storage driver. Verify the selected runtime and StorageClass together on the
worker pool that will run Codespaces.

## Prepare a workload runtime

Codespace runs Docker inside each Runtime Pod. The chart therefore uses an
existing nested-container RuntimeClass instead of modifying worker nodes during
installation.

| Runtime | Isolation | Storage requirement |
| --- | --- | --- |
| Kata Containers | Virtual machine | Raw block PVC |
| Sysbox | User namespace | Filesystem PVC |

### Kata Containers

Install a pinned release using the
[Kata installation guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md)
or its upstream OCI chart. Verify the class with a test Pod on the intended
workers, then label it for Codespace discovery:

```sh
kubectl label runtimeclass kata-qemu-runtime-rs \
  codespace.gitea.dev/isolation=kata \
  --overwrite
```

Kata templates use a StorageClass that provisions raw block volumes.

### Sysbox

Install a pinned release by following the
[Sysbox Kubernetes guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md).
Verify nested Docker in a test Pod and review the
[Sysbox limitations](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/limitations.md)
for workloads that need devices, privileged containers, host networking, or
architecture emulation.

After verifying the intended workers, label the class:

```sh
kubectl label runtimeclass sysbox-runc \
  codespace.gitea.dev/isolation=sysbox \
  --overwrite
```

Sysbox templates use a filesystem PVC.

List the runtime choices visible to Manager with:

```sh
kubectl get runtimeclass -L codespace.gitea.dev/isolation
```

## Install

Create the administration token and install the development chart:

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

Use `--version <chart-version>` for a release instead of the development chart.
Published chart releases reference their matching platform image by digest.
For a private registry, add existing pull Secret names to `image.pullSecrets`.

Verify Manager after installation:

```sh
kubectl -n codespace-system rollout status \
  deployment/codespace-gitea-codespace \
  --timeout=120s
kubectl -n codespace-system get pods
```

## Open the administration page

The administration Service is a `ClusterIP` by default. Open it locally with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Visit <http://127.0.0.1:18080> and sign in with the administration token.
Configure `admin.ingress` when the administration page needs a shared cluster
entry point; the default private Service keeps initial setup off the public
network.

Configure the installation in this order so each later resource can select an
already verified dependency:

1. create a Gateway and any optional Cache components;
2. create an environment template using a verified RuntimeClass and
   StorageClass;
3. create a registration token from Gitea's Codespace Managers page;
4. add the Gitea site using its URL and registration token;
5. assign the template and components, then configure optional quotas; and
6. enable the site after its status reports that it is ready.

## Identity and trust

Manager keeps its internal certificate authority in
`Secret/<release>-internal-ca`. Back up this Secret and enable Kubernetes Secret
encryption at rest. Restoring the same Secret preserves trust between Manager
and existing workloads. Public HTTPS uses the TLS configuration selected for
the Gateway Ingress.

When Gitea uses a private CA, add its PEM bundle to the site. Manager uses it
for site communication and makes it available to Git and the Dev Container.
Leave the field empty for a publicly trusted Gitea certificate.

## Network access

### Gateway publication

Each Gateway has a same-name `ClusterIP` Service: port `80` carries HTTP and
WebSocket traffic, and port `22` carries SSH and SFTP. Publish HTTP through the
configured Ingress or an operator-managed route. Publish SSH through the
optional LoadBalancer Service or an operator-managed TCP route. This leaves
public DNS, TLS, and load-balancer policy under cluster operator control.

Set trusted proxy CIDRs only for proxies that connect directly to the Gateway
Service. Gateway then accepts their forwarded client address without trusting
headers from arbitrary callers.

Gateway replicas share current routing state. Connection limits apply to each
replica rather than to the deployment as a whole.

### Outbound proxies

An environment proxy applies to Runtime and Dev Container traffic. A Cache
proxy applies to registry and S3 traffic from that Cache. They are configured
separately because shared Cache traffic may use a different network path.

Proxy URLs include the scheme and port and may include credentials. Add bypass
entries for destinations that must connect directly and add a CA bundle when
the proxy intercepts TLS. Manager automatically keeps Kubernetes Service and
loopback traffic out of the proxy.

If a template uses a proxy IP address, allow its CIDR and port in every site
that selects the template so the namespace NetworkPolicy permits the traffic.

## Cache

Each Cache has a same-name internal Service on port `443`. Cache traffic stays
inside the cluster and uses the Manager internal CA. An unavailable or empty
Cache falls back to an allowed upstream registry.

Cache supports persistent local storage or S3. It uses one replica with a
Recreate update strategy because cache maintenance has one writer.

## Environment templates

The administration page discovers runtime, storage, Ingress, and certificate
resources from the cluster. An environment template selects those resources
and defines workspace storage, compute limits, proxy settings, and platform
additions to the repository Dev Container.

Templates can enable the Web IDE and provide options for
`ghcr.io/coder/devcontainer-features/code-server:2`. Repository
`customizations.vscode` remains the source for project-specific editor settings
and extensions.

Templates can define a lifecycle script. Codespace invokes it as root with
`build` while constructing the image, `start` before each managed start, and
`stop` before each managed stop. The script is stored in the Codespace template
snapshot, so existing environments retain the behavior they were created with.

[`values.yaml`](values.yaml) contains the chart defaults, and
[`values.schema.json`](values.schema.json) defines all supported Helm values.
Source changes and validation commands are documented in the
[developer guide](../../DEVELOPMENT.md).
