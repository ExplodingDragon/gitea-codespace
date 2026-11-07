# Gitea Codespace Helm chart

This chart installs Codespace Manager, its CRDs, RBAC, administration Service,
and NetworkPolicy. Manager then creates the Gateway, Cache, and Runtime
workloads configured through its administration page.

## Requirements

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- a Gitea installation with Codespace support;
- a Kubernetes Secret containing the administration token;
- a verified Kata Containers or Sysbox RuntimeClass; and
- a StorageClass compatible with that runtime.

Runtime support depends on the worker operating system, architecture, container
runtime, and storage driver. Validate the RuntimeClass and StorageClass together
on the worker pool before assigning them to users.

## Prepare a workload runtime

Each Codespace runs Docker inside its Runtime Pod, so the cluster needs a
nested-container RuntimeClass. The chart uses an existing RuntimeClass and does
not change worker nodes.

| Runtime | Isolation | Workspace volume |
| --- | --- | --- |
| Kata Containers | Virtual machine | Raw block PVC |
| Sysbox | User namespace | Filesystem PVC |

### Kata Containers

Install a pinned release using the
[Kata installation guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md)
or its upstream OCI chart. Run a test Pod on the intended workers, then label
the verified RuntimeClass so Manager can discover it:

```sh
kubectl label runtimeclass kata-qemu-runtime-rs \
  codespace.gitea.dev/isolation=kata \
  --overwrite
```

Select a StorageClass that provisions raw block volumes for Kata templates.

### Sysbox

Install a pinned release using the
[Sysbox Kubernetes guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md).
Verify nested Docker in a test Pod and review the
[Sysbox limitations](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/limitations.md)
for workloads that need devices, privileged containers, host networking, or
architecture emulation. Label the verified RuntimeClass:

```sh
kubectl label runtimeclass sysbox-runc \
  codespace.gitea.dev/isolation=sysbox \
  --overwrite
```

Select a filesystem StorageClass for Sysbox templates. Confirm the resources
visible to Manager with:

```sh
kubectl get runtimeclass -L codespace.gitea.dev/isolation
kubectl get storageclass
```

## Install

Create the administration token before installing the chart. Keeping the token
in a Secret prevents it from appearing in Helm values and release history.

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

This installs the current `main` chart. For a release, add `--version
<chart-version>` and omit `--devel`. Published releases select their matching
platform image by digest. Add existing registry Secret names to
`image.pullSecrets` when the image registry requires authentication.

Verify the installation:

```sh
kubectl -n codespace-system rollout status \
  deployment/codespace-gitea-codespace \
  --timeout=120s
kubectl -n codespace-system get pods
```

## Configure Manager

The administration Service is private by default. Open it from an operator
workstation with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Visit <http://127.0.0.1:18080> and sign in with the administration token. The
initial configuration follows the dependency order shown in the interface:

1. create a Gateway and any optional Cache components;
2. create an environment template from a verified RuntimeClass and
   StorageClass;
3. create a registration token in Gitea's Codespace Managers page;
4. register the Gitea site with its URL and token;
5. assign the template and components, configure quotas when needed, and enable
   the site after it becomes ready.

To publish the administration page, configure `admin.ingress`. When the chart
NetworkPolicy is enabled, add the directly connecting ingress source networks
to `networkPolicy.adminIngressCIDRs`; this allows the ingress controller to
reach the otherwise private administration port.

## Trust and networking

Manager stores its internal certificate authority in
`Secret/<Helm fullname>-internal-ca`. Back up this Secret and enable Kubernetes
Secret encryption at rest. Restoring it preserves trust between Manager and
existing workloads. When Gitea uses a private CA, add that PEM bundle to the
site configuration so Manager and Dev Containers trust it.

Each Gateway has a same-name ClusterIP Service. Port `80` carries HTTP and
WebSocket traffic, while port `22` carries SSH and SFTP. Publish HTTP through
the configured Ingress or an operator-managed route. Publish SSH through the
optional LoadBalancer Service or an operator-managed TCP route. Public DNS, TLS,
and load-balancer policy therefore remain under cluster operator control.

Set Gateway trusted proxy CIDRs only for proxies that connect directly to its
Service. Environment and Cache proxies are configured separately in Manager
because workspace traffic and shared registry traffic can use different network
paths. If an environment proxy uses an IP address, allow its CIDR and port in
each site that uses the template so the site NetworkPolicy permits the traffic.

Cache Services remain inside the cluster on port `443` and use Manager's
internal CA. A persistent local Cache runs as one replica with a Recreate update
strategy; S3 can be selected instead when shared object storage is preferred.

## Chart configuration

[`values.yaml`](values.yaml) lists the chart defaults and
[`values.schema.json`](values.schema.json) defines the accepted values. Reuse
the same `helm upgrade --install` command for upgrades so Helm preserves the
release and applies the selected image version consistently.

Source changes and validation commands are documented in the
[contributor guide](../../DEVELOPMENT.md).
