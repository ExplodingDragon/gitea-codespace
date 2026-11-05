# Gitea Codespace Helm chart

This chart installs the Codespace Manager, CRDs, RBAC, internal Services, and
NetworkPolicy. Manager creates Gateway, Cache, and Runtime workloads from the
resources configured in the administration page.

## Requirements

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- a Gitea installation with Codespace support;
- an administrator token stored in a Kubernetes Secret;
- a verified Kata Containers or Sysbox RuntimeClass; and
- a StorageClass compatible with the selected runtime.

Runtime support also depends on the worker operating system, architecture,
CRI, and container runtime. Verify the selected runtime on the same worker pool
and storage used by Codespaces.

## Prepare a workload runtime

Codespace runs Docker inside each Runtime Pod, then runs the Dev Container in
that Docker environment. Kata provides VM-backed isolation and uses block
storage. Sysbox provides user-namespace isolation and uses filesystem storage.
The chart consumes an existing RuntimeClass because installing either runtime
changes the worker nodes and remains a cluster operation.

### Kata Containers

Install a pinned release using the
[Kata installation guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md)
or its upstream OCI chart. Run a test Pod with the selected class and confirm
that it reports a guest kernel, then expose the verified class to Codespace:

```sh
kubectl label runtimeclass kata-qemu-runtime-rs \
  codespace.gitea.dev/isolation=kata \
  --overwrite
```

Kata templates require a StorageClass that provisions raw block volumes. Test
the RuntimeClass and StorageClass together because Kubernetes StorageClass
metadata alone does not describe every mode supported by its CSI driver.

### Sysbox

Install a pinned Sysbox release by following the
[Sysbox Kubernetes guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md).
Run a Pod with `sysbox-runc` and confirm that its nested Docker daemon responds.
Review the [Sysbox limitations](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/limitations.md)
when templates need devices, privileged containers, host networking, or
multi-architecture emulation.

After verifying the intended workers, label the class:

```sh
kubectl label runtimeclass sysbox-runc \
  codespace.gitea.dev/isolation=sysbox \
  --overwrite
```

Sysbox templates use a filesystem PVC. Verify that the StorageClass can
provision and remount the volume on the intended worker pool.

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

Use `--version <chart-version>` to install an exact chart release. Published
chart releases reference the platform image produced by the same workflow by
digest. For a private registry, add existing pull Secret names to
`image.pullSecrets`; Manager makes those credentials available to managed
site namespaces.

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

Complete the initial setup in this order:

1. create a Gateway and any optional Cache components;
2. create an environment template using a verified RuntimeClass and
   StorageClass;
3. create a registration token from Gitea's Codespace Managers page;
4. add the Gitea site using its URL and registration token;
5. assign the template and components, then configure optional quotas; and
6. enable the site after its status reports that it is ready.

## Identity and trust

Manager keeps the internal root and issuing CA in
`Secret/<release>-internal-ca`. It secures Manager, Agent, Gateway, Cache, and
Cache HTTPS communication. Back up this Secret and enable Kubernetes Secret
encryption at rest because restoring the same identity lets existing workloads
retain their trust after Manager recovery. Public HTTPS uses the TLS Secret or
cert-manager configuration selected for the corresponding Ingress.

When Gitea uses a private CA, add its PEM bundle to the site. Manager uses it
for site communication and makes it available to Git and the Dev Container.
Leave the field empty for a publicly trusted Gitea certificate.

## Network access

### Gateway publication

Each Gateway has a same-name `ClusterIP` Service. Port `80` carries HTTP and
WebSocket traffic, while port `22` carries SSH and SFTP. HTTP can use the
Ingress configured in the administration page or an operator-managed route.
SSH can use the optional LoadBalancer Service or an operator-managed TCP
route. These choices keep public DNS, TLS, and load-balancer policy under the
cluster operator's control.

Set trusted proxy CIDRs only for proxies that connect directly to the Gateway
Service. Gateway then accepts their forwarded client address without trusting
headers from arbitrary callers.

Gateway replica count is configured on the component. Multiple replicas share
current routing state and are distributed across nodes when possible;
connection limits apply to each replica.

### Outbound proxies

An environment-template proxy applies to Git over HTTP(S), image pulls and
builds, Dev Container Features, and applications that use standard proxy
environment variables. A Cache proxy applies to registry and S3 traffic from
that Cache. Separate settings allow Runtime and shared Cache traffic to use
different network paths.

Proxy URLs include the scheme and port and may include credentials. Add bypass
entries for destinations that must connect directly and add a CA bundle when
the proxy intercepts TLS. Manager automatically keeps Kubernetes Service and
loopback traffic out of the proxy.

If a template uses a proxy IP address, allow its CIDR and port in every site
that selects the template so the namespace NetworkPolicy permits the traffic.

## Cache

Each Cache has a same-name internal Service on port `443`. Runtime Docker and
BuildKit verify it with the Manager internal CA and use short-lived scoped
credentials. Cache traffic stays inside the cluster; an unavailable or empty
Cache falls back to the allowed upstream registry.

Cache supports local persistent storage or S3. Configure its upstream allow
rules and optional proxy in the administration page. It uses one replica with
a Recreate update strategy because the storage and maintenance lease have one
writer.

## Environment templates

The administration page discovers runtime, storage, Ingress, and certificate
resources from the cluster. Templates select from those verified resources and
define workspace storage, optional compute limits, proxy settings, and
platform additions to the repository Dev Container.

Environment templates can enable the platform Web IDE and pass an options
object to `ghcr.io/coder/devcontainer-features/code-server:2`. This keeps the
upstream Feature options available without duplicating its version or extension
fields in the administration API. Codespace supplies the internal listener,
authentication, workspace, TLS boundary, telemetry, and update-check options
because those values must match Gateway and the Codespace lifecycle.

Repository `customizations.vscode` remains the project-owned source of editor
settings and extensions. Feature options can provide an administrator baseline,
while repositories add project-specific tools. Existing Codespaces retain
their persistent editor state when resumed.

[`values.yaml`](values.yaml) contains the chart defaults, and
[`values.schema.json`](values.schema.json) defines all supported Helm values.
Source changes and validation commands are documented in the
[developer guide](../../DEVELOPMENT.md).
