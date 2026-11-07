# Gitea Codespace Helm chart

This chart installs Codespace Manager and the Kubernetes resources it needs.
Manager creates the Gateway, Cache, and Runtime workloads configured through
its administration page.

## Requirements

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- a Gitea installation with Codespace support;
- a Kubernetes Secret containing the administration token;
- a verified Kata Containers or Sysbox RuntimeClass; and
- a StorageClass compatible with that runtime.

Runtime support depends on the worker operating system, architecture, container
runtime, and storage driver. Test the RuntimeClass and StorageClass together on
the intended worker pool before assigning them to users.

## Prepare a workload runtime

Each Codespace runs Docker inside its Runtime Pod, so the cluster needs a
nested-container RuntimeClass. Install and verify that runtime on the worker
nodes before configuring Manager.

| Runtime | Isolation | Workspace volume |
| --- | --- | --- |
| Kata Containers | Virtual machine | Raw block PVC |
| Sysbox | User namespace | Filesystem PVC |

### Kata Containers

Install a pinned release using the
[Kata installation guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md)
or its upstream OCI chart. Run a test Pod on the intended workers, then label
the verified RuntimeClass so it appears in Manager:

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

Create the administration token as a Secret so it does not appear in Helm
values or release history.

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

This installs the current `main` chart. To install a release, add `--version
<chart-version>` and omit `--devel`. A published release selects its matching
platform image by digest. Set `image.pullSecrets` when the image registry
requires authentication.

Verify the installation:

```sh
kubectl -n codespace-system rollout status \
  deployment/codespace-gitea-codespace \
  --timeout=120s
kubectl -n codespace-system get pods
```

## Configure Manager

The administration Service is a ClusterIP by default. Access it from an
operator workstation with:

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

To publish the administration page, configure `admin.ingress`. If the chart
NetworkPolicy is enabled, add the ingress controller's source networks to
`networkPolicy.adminIngressCIDRs` so it can reach the administration Service.

## Trust and networking

Manager stores its internal certificate authority in
`Secret/<Helm fullname>-internal-ca`. Back up this Secret because it preserves
trust between Manager and existing workloads, and enable Kubernetes Secret
encryption at rest. When Gitea uses a private CA, add its PEM bundle to the site
configuration so Manager and Dev Containers trust it.

Each Gateway has a same-name ClusterIP Service. Port `80` carries HTTP and
WebSocket traffic, while port `22` carries SSH and SFTP. Publish HTTP with the
configured Ingress or an operator-managed route. Publish SSH with the optional
LoadBalancer Service or an operator-managed TCP route. The cluster operator
remains responsible for public DNS, TLS, and load-balancer policy.

Configure Gateway trusted proxy CIDRs for proxies that connect directly to its
Service. Environment and Cache proxies are separate because workspace and
registry traffic can use different network paths. If an environment proxy uses
an IP address, add its CIDR and port to every affected site so the site
NetworkPolicy permits that traffic.

Cache Services use Manager's internal CA and remain inside the cluster on port
`443`. Use the local backend for a single persistent Cache or S3 for shared
object storage.

Manager, Gateway, and Cache expose Kubernetes probes on plain HTTP port `8081`
inside their Pods. This port is only for kubelet health checks and is not
published through a Service or Ingress.

## Chart configuration

[`values.yaml`](values.yaml) lists the chart defaults and
[`values.schema.json`](values.schema.json) defines the accepted values. Use
`helm upgrade --install` for upgrades so Helm preserves the release and applies
the selected image version consistently.

Source changes and validation commands are documented in the
[contributor guide](../../DEVELOPMENT.md).
