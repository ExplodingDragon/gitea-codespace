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

Runtime support depends on the worker operating system, architecture, CRI, and
container runtime as well as the Kubernetes version. Check the runtime's
support matrix before changing worker nodes.

## Prepare a workload runtime

Codespace runs Docker inside each Runtime Pod and runs the Dev Container in
that Docker environment. Kata provides VM-backed isolation and needs block
storage. Sysbox provides user-namespace isolation and uses filesystem storage.
The chart uses an existing RuntimeClass so runtime installation and node
maintenance remain under the cluster operator's control.

### Kata Containers

Install a supported, pinned Kata release by following the
[Kata installation guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md).
The upstream OCI chart is one supported installation method:

```sh
helm upgrade --install kata-deploy \
  oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy \
  --version <supported-release> \
  --namespace kube-system \
  --create-namespace \
  --set shims.disableAll=true \
  --set shims.qemu-runtime-rs.enabled=true \
  --wait \
  --timeout 10m
```

Run a test Pod with the selected class and confirm that it reports a guest
kernel. Then make the verified class available to the administration page:

```sh
kubectl label runtimeclass kata-qemu-runtime-rs \
  codespace.gitea.dev/isolation=kata \
  --overwrite
```

The selected StorageClass must provision a raw block volume for Kata. Test the
RuntimeClass and StorageClass together before publishing a template because a
StorageClass object does not advertise every volume mode supported by its CSI
driver.

### Sysbox

Install a supported, pinned Sysbox release by following the
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

Sysbox templates use a filesystem PVC. Verify that the chosen StorageClass can
provision and remount that volume on the worker pool.

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

Verify Manager and its installation identity:

```sh
kubectl -n codespace-system rollout status \
  deployment/codespace-gitea-codespace \
  --timeout=120s
kubectl -n codespace-system get secret \
  codespace-gitea-codespace-internal-ca
kubectl -n codespace-system get pods
```

## Open the administration page

The administration Service is a `ClusterIP` by default. Open it locally with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Visit <http://127.0.0.1:18080> and sign in with the administration token. To
publish the page, configure `admin.ingress`; it can remain HTTP behind a
trusted proxy or reference an existing TLS Secret.

Complete the initial setup in this order:

1. create a Gateway and any optional Cache components;
2. create an environment template using a verified RuntimeClass and
   StorageClass;
3. create a registration token from Gitea's Codespace Managers page;
4. add the Gitea site using its URL and registration token;
5. assign the template and components, then configure optional quotas; and
6. enable the site after its status reports that it is ready.

## Certificates and recovery

Manager stores its internal root and issuing CA in
`Secret/<release>-internal-ca`. This identity secures Manager, Agent, Gateway,
and Cache communication and Cache HTTPS. Back up the Secret and enable
Kubernetes Secret encryption at rest. It is intentionally retained across
ordinary Helm upgrades so existing workloads keep the same trust root.

Restore the original Secret if Manager reports that the identity is missing
while managed resources still exist. Create a new identity only for an empty
installation. Browser-facing HTTPS remains separate and can use an existing
TLS Secret or cert-manager through the Gateway configuration.

When Gitea uses a private CA, add its PEM bundle to the site. Manager uses it
for site communication and makes it available to Git and the Dev Container.
Leave the field empty for a publicly trusted Gitea certificate.

## Network and proxy configuration

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

## Gateway publication

Each Gateway has a same-name `ClusterIP` Service. Port `80` carries HTTP and
WebSocket traffic; port `22` carries SSH and SFTP. Users can also use direct
commands, VS Code Remote-SSH, and localhost TCP forwarding through SSH.

HTTP can remain operator-managed or use an Ingress configured in the
administration page. HTTPS references an existing TLS Secret or a cert-manager
Issuer. SSH can remain operator-managed or use the separate LoadBalancer
Service created by Manager. In operator-managed mode, route external HTTP and
SSH traffic to ports `80` and `22` of the Gateway Service.

Only configure trusted proxy CIDRs when an HTTP proxy connects directly to the
Gateway Service. This allows Gateway to use the real client address without
trusting forwarded headers from arbitrary callers.

Gateway replica count is configured on the component. Manager distributes
multiple replicas across nodes when possible and keeps their sessions
interoperable. Connection limits apply per replica. Cache components use one
replica because their storage and maintenance lease have a single writer.

## Cache

Each Cache has a same-name internal Service on port `443`. Runtime Docker and
BuildKit verify it with the Manager internal CA and use short-lived scoped
credentials. Cache traffic stays inside the cluster; an unavailable or empty
Cache falls back to the allowed upstream registry.

Cache supports local persistent storage or S3. Configure its upstream allow
rules and optional proxy in the administration page. The component uses one
replica with a Recreate update strategy so only one process writes its storage
at a time.

## Configuration reference

The administration page discovers RuntimeClasses, StorageClasses,
IngressClasses, TLS Secrets, and cert-manager Issuers from the cluster. It also
validates resource requests, optional limits, quotas, component relationships,
and referenced objects before applying changes.

Environment templates can define the initial Web IDE version, extensions, and
settings. Repository `customizations.vscode` values refine that configuration
for newly created Codespaces; existing Codespaces keep their persistent IDE
state when resumed.

[`values.yaml`](values.yaml) contains the chart defaults, and
[`values.schema.json`](values.schema.json) defines all supported Helm values.
Source changes and validation commands are documented in the
[developer guide](../../DEVELOPMENT.md).
