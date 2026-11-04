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

The chart references existing RuntimeClasses and leaves node configuration to
the cluster operator. Install Kata using its
[installation guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md),
or install Sysbox on a dedicated node pool using its
[Kubernetes guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md).
Verify the selected runtime, storage, and platform image with a real Pod first.
Label the verified RuntimeClass so Manager can present it with the correct
isolation mode:

```sh
kubectl label runtimeclass kata-qemu codespace.gitea.dev/isolation=kata
kubectl label runtimeclass sysbox-runc codespace.gitea.dev/isolation=sysbox
```

The broader isolation, storage, network, identity, and recovery design is
defined in the [deployment requirements](https://github.com/ExplodingDragon/gitea-dev/blob/main/src/codespace/deployment-requirements.md).

## Internal identity

Manager maintains an installation-specific root and issuing CA in
`Secret/<release>-internal-ca`. It directly issues 24-hour certificates for
Manager, Agent, Gateway, and Cache control connections. It also issues a
separate HTTPS server certificate for each Cache Service DNS name. The root
remains stable while the issuing CA and workload certificates rotate, so new
TLS connections use the renewed files without restarting workloads or
requiring another cluster service. File-backed identities are validated at
startup and cached in memory. New connections check the projected files at
most once per minute, so normal traffic does not read certificate files for
every TLS handshake.

The root is valid for ten years. The issuing CA is valid for one year and is
replaced 30 days before expiry. Workload certificates are replaced eight hours
before their 24-hour expiry. These intervals keep the stable recovery boundary
separate from routinely rotated online keys while leaving enough time for a
temporary control-plane outage.

The CA Secret is created through the Kubernetes API after Manager starts; the
Manager Pod does not mount a pre-existing identity Secret. Multiple replicas
use Kubernetes object creation and resource versions to converge on the same
CA. Include this Secret in cluster backups and enable Kubernetes Secret
encryption at rest. If the Secret is lost while managed resources exist,
Manager stops instead of silently creating a different trust hierarchy.

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
`ghcr.io/explodingdragon/gitea-codespace:main` when installed directly from a
checkout. Pushes to `main` publish a Helm prerelease named
`0.1.0-main.<run-number>`. CI packages a temporary chart copy whose
`image.digest` is the immutable digest produced by the same workflow. This
keeps the source convenient for development while ensuring that an OCI chart
version always installs its corresponding image. For a reproducible
installation, use the exact chart version from the workflow summary:

```sh
helm upgrade --install codespace \
  oci://ghcr.io/explodingdragon/charts/gitea-codespace \
  --version 0.1.0-main.REPLACE_WITH_RUN_NUMBER \
  --namespace codespace-system \
  --set admin.tokenSecret.name=codespace-admin \
  --wait \
  --timeout 5m
```

Tagged platform images use the Kubernetes `Always` pull policy. Digest-pinned
images use `IfNotPresent`. For a private registry, list existing pull Secret
names in `image.pullSecrets`; Manager copies only those credentials to managed
site namespaces.

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
registry address.
The Manager renews the certificate before expiry. Cache keeps the last valid
key pair in memory and checks the projected files at most once per minute when
new TLS connections arrive. A successful check replaces the complete key pair;
a failed check keeps the previous pair only while its certificate remains
valid.

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

Creating a replacement CA is appropriate only for an empty installation. This
distinction prevents a deleted Secret from turning into an undetected second
trust domain.

## Configuration and validation

The administration page also discovers labeled RuntimeClasses and available
StorageClasses. CPU, memory, storage, cache limits, and durations are entered
as a number with an explicit unit; Manager stores the corresponding native
Kubernetes quantity or duration and validates request/limit and cache capacity
relationships before applying resources. Environment templates require CPU and
memory requests but can omit limits. Site quotas are optional; when disabled,
Manager does not maintain a ResourceQuota for that site namespace.

[`values.yaml`](values.yaml) contains defaults. [`values.schema.json`](values.schema.json)
is the authoritative list of supported values and validation rules. Render and
validate the chart through the repository Makefile:

```sh
make helm-check
```
