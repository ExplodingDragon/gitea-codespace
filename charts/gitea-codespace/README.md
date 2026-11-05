# Gitea Codespace Helm chart

This chart installs the Codespace Manager control plane: CRDs, RBAC, a
multi-replica Manager Deployment, internal and administration Services, and
NetworkPolicy. Gateway, Cache, and Runtime workloads are created later from
objects configured through the administration UI.

## Prerequisites

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- access to the Gitea Codespace platform image;
- an administrator token stored in a Kubernetes Secret;
- at least one verified Kata Containers or Sysbox RuntimeClass labeled with
  `codespace.gitea.dev/isolation=kata|sysbox`, and a compatible StorageClass
  before an environment template is enabled.

The Codespace Kubernetes baseline and the selected runtime's upstream support
matrix both apply. Meeting the Kubernetes version above does not imply that a
particular Kata or Sysbox release supports the cluster's operating system,
architecture, CRI, or container runtime.

## Install a workload runtime

Install Kata Containers or Sysbox before installing this chart. Installing
both is optional; separate environment templates can select different runtimes
when both are available. The Codespace chart consumes existing RuntimeClasses
rather than changing worker nodes because runtime installation writes host
artifacts and updates containerd or another CRI implementation. Keeping that
lifecycle with the cluster operator also allows the runtime to be shared by
other workloads without coupling it to a Codespace release.

| Runtime | Best suited for | Worker-node requirement | Codespace storage |
| --- | --- | --- | --- |
| Kata Containers | Lightweight virtual-machine isolation | Supported hardware virtualization, architecture, CRI, and Kata release | A StorageClass that can provision raw block volumes |
| Sysbox | Nested Docker without hardware virtualization | A combination supported by the current Sysbox Kubernetes, OS, architecture, and containerd matrix | A filesystem volume |

In both modes, the Runtime Pod starts its own Docker daemon and runs the actual
Dev Container inside that Docker environment.

### Install Kata Containers

Kata recommends its OCI Helm chart for Kubernetes. Pin a release so the node
artifacts, RuntimeClasses, and Helm release remain reproducible:

```sh
export KATA_VERSION="<supported-release>"

helm upgrade --install kata-deploy \
  oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy \
  --version "${KATA_VERSION}" \
  --namespace kube-system \
  --create-namespace \
  --set shims.disableAll=true \
  --set shims.qemu-runtime-rs.enabled=true \
  --wait \
  --timeout 10m
```

Select a version and any distribution-specific values from the
[Kata installation guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md).
The selected workers must expose supported hardware virtualization. The
example installs `kata-qemu-runtime-rs`, the current runtime-rs QEMU class used
by the Codespace test suite.

Kata Deploy uses short-lived per-node Jobs by default. This minimizes
long-lived privileged access on clusters with a stable worker set. Clusters
that add worker nodes automatically can use `deploymentMode=daemonset` or the
scheduled reconciliation described in the
[Kata Helm configuration](https://github.com/kata-containers/kata-containers/blob/main/docs/helm-configuration.md),
so newly added nodes receive the runtime before Kata workloads are scheduled.

Confirm that the RuntimeClass starts a VM-backed Pod:

```sh
kubectl get runtimeclass kata-qemu-runtime-rs

cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: codespace-kata-check
spec:
  runtimeClassName: kata-qemu-runtime-rs
  restartPolicy: Never
  containers:
    - name: check
      image: quay.io/libpod/ubuntu:latest
      command: ["/bin/sh", "-c"]
      args: ["uname -r; sleep 30"]
EOF

kubectl wait --for=condition=Ready pod/codespace-kata-check --timeout=2m
kubectl logs codespace-kata-check
kubectl delete pod codespace-kata-check
```

The reported kernel should be the Kata guest kernel rather than the worker
node's kernel. Record the successful verification for Manager by labeling the
class:

```sh
kubectl label runtimeclass kata-qemu-runtime-rs \
  codespace.gitea.dev/isolation=kata \
  --overwrite
```

### Install Sysbox

Use the current
[Sysbox Kubernetes installation guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md)
to confirm the supported Kubernetes, Ubuntu, architecture, and containerd
combination before changing a worker node. The Codespace Kubernetes minimum
and the upstream Sysbox support matrix must both cover the selected workers.
Review the installer before applying it because runtime installation can
change the node's CRI configuration and restart kubelet.

Choose the workers that will host Sysbox Codespaces, then apply a reviewed and
pinned Sysbox manifest instead of relying on a moving branch:

```sh
kubectl label node <worker-node> sysbox-install=yes

export SYSBOX_REF="<release-tag-or-reviewed-commit>"

kubectl apply -f \
  "https://raw.githubusercontent.com/nestybox/sysbox/${SYSBOX_REF}/sysbox-k8s-manifests/sysbox-install.yaml"

kubectl -n kube-system rollout status daemonset/sysbox-deploy-k8s \
  --timeout=10m
kubectl get runtimeclass sysbox-runc
```

Confirm that the RuntimeClass supports the user-namespace and nested-Docker
model used by Codespace:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: codespace-sysbox-check
spec:
  runtimeClassName: sysbox-runc
  hostUsers: false
  restartPolicy: Never
  containers:
    - name: check
      image: registry.nestybox.com/nestybox/ubuntu-focal-systemd-docker
      command: ["/sbin/init"]
EOF

kubectl wait --for=condition=Ready pod/codespace-sysbox-check --timeout=2m
kubectl exec codespace-sysbox-check -- /bin/sh -c \
  'for i in $(seq 1 60); do docker info && exit 0; sleep 1; done; exit 1'
kubectl delete pod codespace-sysbox-check
```

Review the upstream
[Sysbox limitations](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/limitations.md)
when templates need host devices, multi-architecture emulation, privileged
containers, host networking, or nested user namespaces. These capabilities
have different support boundaries from a VM-backed Kata environment.

After the check succeeds, label the RuntimeClass:

```sh
kubectl label runtimeclass sysbox-runc \
  codespace.gitea.dev/isolation=sysbox \
  --overwrite
```

### Verify storage compatibility

Kata templates use the Codespace data PVC as a raw block device. Their
StorageClass must support `volumeMode: Block` together with
`ReadWriteOncePod`, or a `ReadWriteOnce` combination that the cluster operator
has verified. Sysbox templates use a filesystem PVC. A StorageClass object does
not advertise whether its CSI driver supports every volume mode, so validate
the selected runtime and StorageClass together with a real test PVC before
publishing an environment template.

### Runtime installation readiness

The cluster is ready for the Codespace chart when:

- a test Pod starts with the selected RuntimeClass on every intended worker
  pool;
- a nested Docker daemon responds in the Sysbox check, or the Kata check
  reports a guest kernel;
- the selected StorageClass provisions the volume mode required by that
  runtime;
- the verified RuntimeClass carries the corresponding isolation label; and
- the following command shows the expected class and isolation value:

```sh
kubectl get runtimeclass \
  -L codespace.gitea.dev/isolation
```

The isolation label records the operator's verification and lets Manager
filter the administration form. Runtime installation and CRI handler
registration remain node preparation steps. Create a real Codespace from the
finished environment template as the final platform-image, storage, network,
and nested-Docker check.

The broader isolation, storage, network, identity, and recovery design is
defined in the [deployment requirements](https://github.com/ExplodingDragon/gitea-dev/blob/main/src/codespace/deployment-requirements.md).

## Internal identity

Manager maintains an installation-specific root and issuing CA in
`Secret/<release>-internal-ca`. It issues certificates for Manager, Agent,
Gateway, and Cache control connections, plus a separate HTTPS server
certificate for each Cache Service. The root remains stable while online
certificates rotate.

Include the CA Secret in cluster backups and enable Kubernetes Secret
encryption at rest. Manager stops when this identity is missing while managed
resources still exist, allowing an operator to restore the original trust
root.

Cache control identities and Cache Registry server certificates are stored in
separate Secrets and carry different certificate purposes. Runtime Pods receive
only the public root certificate and use it to verify Cache HTTPS; the CA
private keys remain in the Manager-owned CA Secret. This internal CA is
separate from browser-facing HTTPS. cert-manager is optional and can populate
the TLS Secret referenced by a managed Ingress.

## Install

Create the administration token and install the newest development chart:

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

The chart source defaults to
`ghcr.io/explodingdragon/gitea-codespace:main`. Published OCI chart versions
pin the platform image produced by the same workflow to an immutable digest.
Use an exact chart version for a reproducible installation:

```sh
helm upgrade --install codespace \
  oci://ghcr.io/explodingdragon/charts/gitea-codespace \
  --version 0.1.0-main.REPLACE_WITH_RUN_NUMBER \
  --namespace codespace-system \
  --set admin.tokenSecret.name=codespace-admin \
  --wait \
  --timeout 5m
```

For a private registry, list existing pull Secret names in
`image.pullSecrets`; Manager copies those credentials to managed site
namespaces.

## Verify the installation

Wait for Manager and verify that the installation CA exists:

```sh
kubectl -n codespace-system rollout status \
  deployment/codespace-gitea-codespace \
  --timeout=120s
kubectl -n codespace-system get secret \
  codespace-gitea-codespace-internal-ca
kubectl -n codespace-system get pod
```

The internal CA Secret is not owned by the Helm release because it must survive
ordinary upgrades and reinstalling the same release. Keep the release resource
name stable while managed resources exist. Deleting the management namespace
removes the installation and its trust root.

## Administration access and HTTPS

The administration Service defaults to `ClusterIP`. Open a local session with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

To publish the page, enable `admin.ingress`. The host and TLS Secret are
optional, so the Ingress can use a controller default host, terminate TLS with
an existing Secret, or remain HTTP behind another trusted proxy. The chart
only references a configured TLS Secret and does not couple public TLS to the
internal component CA. The page uses same-origin API requests, so switching
between port-forward, a reverse proxy, and Ingress does not require a Manager
public URL setting.

## Gitea site certificates

Leave the site CA field empty when Gitea uses a publicly trusted certificate.
For an HTTPS Gitea installation with an internal CA, paste its PEM CA bundle
into the site form. Manager adds that bundle to the system trust roots for
registration and coordination, and passes it to the Runtime Agent for Git and
the primary Dev Container. This site-specific bundle is separate from both
the internal component CA and the public Gateway certificate.

## Cache access

Each Cache component creates a same-name ClusterIP Service on port `443` and a
`<component>-registry-tls` Secret for that Service DNS name. The Cache process
listens on container port `8443`, and the Service DNS name is its canonical
registry address. Manager renews its certificate before expiry.

Runtime Docker installs the public internal root under the Cache host in its
`certs.d` directory. BuildKit and Docker therefore use standard HTTPS rather
than an insecure registry exception. Every Registry request also uses a
short-lived Manager-signed credential. Mirror paths allow pulls, while build
cache paths allow the scoped pull and push operations needed by the current
Codespace.

## Gateway publication

Every Gateway component has a same-name ClusterIP Service. Port `80` forwards
HTTP and WebSocket traffic to the Gateway HTTP process, while port `22`
forwards SSH and SFTP traffic. This stable Service is the boundary between
Codespace authorization and cluster networking, so changing the public entry
point does not replace the Gateway workload or expose individual Codespaces.

HTTP publication can stay under operator control or use a Manager-owned
Ingress. The administration page discovers IngressClasses from the cluster.
For HTTPS, select an existing TLS Secret or an installed cert-manager Issuer;
Manager then configures the wildcard host for the endpoint base domain. The
Gateway process receives plain HTTP from the Service and never mounts the
public TLS private key.

SSH publication can stay under operator control or use a separate
`<component>-ssh` LoadBalancer Service. That Service publishes only port `22`
and preserves the client source address. SSH uses the component's host key,
which is independent from the HTTPS certificate. In operator-managed mode,
route external traffic to ports `80` and `22` of the same-name Service.

Configure trusted proxy CIDRs only when an HTTP proxy connects directly to the
Gateway Service. Gateway ignores forwarded client-address headers from every
other source, which keeps per-address limits from trusting user input.

Gateway replica count is configured on the component in the administration
page, not as a Helm value. Two replicas are the default. Manager creates a
rolling Deployment, a soft cross-node anti-affinity rule, and a
PodDisruptionBudget when more than one replica is selected. All replicas share
the component session key, so an authenticated browser request can move between
ready Pods without a sticky-session setting. Connection limits remain per Pod
and protect each Gateway process independently.

Cache components always use one replica and the Recreate Deployment strategy.
This matches the exclusive maintenance lease and ReadWriteOnce storage model;
the cache is disposable performance data, so Runtime falls back to the allowed
upstream while the Cache Pod is being replaced.

## Internal CA recovery

Back up `Secret/<release>-internal-ca` with the other management resources.
Manager reports an explicit startup error when the Secret is missing or
invalid while managed state exists. Restore the original Secret and restart
Manager; Agent, Gateway, and Cache will reconnect using the same root.

Initialize a new CA only for an empty installation. Installations with managed
resources recover the original CA so existing workload identities remain
valid.

## Configuration and validation

The administration page also discovers labeled RuntimeClasses and available
StorageClasses. CPU, memory, storage, cache limits, and durations are entered
as a number with an explicit unit; Manager stores the corresponding native
Kubernetes quantity or duration and validates request/limit and cache capacity
relationships before applying resources. Environment templates require CPU and
memory requests but can omit limits. Site quotas are optional; when disabled,
Manager does not maintain a ResourceQuota for that site namespace.

[`values.yaml`](values.yaml) contains defaults. [`values.schema.json`](values.schema.json)
is the authoritative list of supported values and validation rules. The
[developer guide](../../DEVELOPMENT.md) documents chart validation for source
changes.
