# Gitea Codespace Helm chart

This chart installs Codespace Manager and its cluster-level Kubernetes
resources. Manager then creates the Gateway, Cache, and Runtime workloads
configured through its administration page.

## Before installation

- Kubernetes 1.31 or later
- Helm 4.3 or later
- Gitea with Codespace support
- A verified Kata Containers or Sysbox RuntimeClass
- A StorageClass suitable for that runtime

Runtime support depends on the worker operating system, architecture, container
runtime, and storage driver. Test the RuntimeClass and StorageClass together on
the intended worker pool before assigning them to users.

## Prepare a workload runtime

Each Codespace runs Docker inside its Runtime Pod. Install and verify a runtime
that supports nested containers before configuring Manager.

| Runtime | Isolation | Workspace volume |
| --- | --- | --- |
| Kata Containers | Virtual machine | Raw block PVC |
| Sysbox | User namespace | Filesystem PVC |

### Kata Containers

Install a pinned release using the
[Kata installation guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md)
or its upstream OCI chart. Verify it on the intended workers, then label the
RuntimeClass so Manager can offer it to environment templates:

```sh
kubectl label runtimeclass kata-qemu-runtime-rs \
  codespace.gitea.dev/isolation=kata \
  --overwrite
```

Use a StorageClass that provisions raw block volumes for Kata templates.

### Sysbox

Install a pinned release using the
[Sysbox Kubernetes guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md).
Verify nested Docker in a test Pod and review the
[Sysbox limitations](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/limitations.md)
for workloads that need devices or elevated container privileges. Label the
verified RuntimeClass:

```sh
kubectl label runtimeclass sysbox-runc \
  codespace.gitea.dev/isolation=sysbox \
  --overwrite
```

Use a filesystem StorageClass for Sysbox templates. Confirm the resources that
Manager will discover:

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

This installs the current `main` chart. For a release, replace `--devel` with
`--version <chart-version>`. Release charts select their matching platform
image by digest. Set `image.pullSecrets` for a private registry.

Verify the installation:

```sh
kubectl -n codespace-system rollout status \
  deployment/codespace-gitea-codespace \
  --timeout=120s
kubectl -n codespace-system get pods
```

## Configure Codespace

The administration Service is a ClusterIP by default. Access it from an
operator workstation with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Visit <http://127.0.0.1:18080> and sign in with the administration token. Create
resources in the order shown by the interface:

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

## Publish workspace access

Each Gateway has a same-name ClusterIP Service. Port `80` carries HTTP and
WebSocket traffic, while port `22` carries SSH and SFTP. Publish HTTP with the
configured Ingress or an operator-managed route. Publish SSH with the optional
LoadBalancer Service or an operator-managed TCP route. The cluster operator
remains responsible for public DNS, TLS, and load-balancer policy.

## Trust and proxies

Back up `Secret/<Helm fullname>-internal-ca`; it preserves trust between Manager
and existing workloads. Enable Kubernetes Secret encryption at rest. When
Gitea uses a private CA, add its PEM bundle to the site configuration so
Manager and Dev Containers trust it.

Configure Gateway trusted proxy CIDRs for proxies that connect directly to its
Service. Environment and Cache proxies are configured separately because
workspace and registry traffic can use different network paths. Add an
environment proxy's CIDR and port to each affected site so its NetworkPolicy
permits the connection.

Cache Services use Manager's internal CA and remain inside the cluster on port
`443`. Use the local backend for a single persistent Cache or S3 for shared
object storage.

## Customize the chart

[`values.yaml`](values.yaml) lists the chart defaults and
[`values.schema.json`](values.schema.json) defines the accepted values. Use
`helm upgrade --install` for upgrades so Helm preserves the release and applies
the selected image version consistently.
