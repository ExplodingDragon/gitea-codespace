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
isolation and volume behavior:

```sh
kubectl label runtimeclass kata-qemu codespace.gitea.dev/isolation=kata
kubectl label runtimeclass sysbox-runc codespace.gitea.dev/isolation=sysbox
```

The broader isolation, storage, network, identity, and recovery design is
defined in the [deployment requirements](https://github.com/ExplodingDragon/gitea-dev/blob/main/src/codespace/deployment-requirements.md).

## Internal identity

Manager maintains an installation-specific root and issuing CA in
`Secret/<release>-internal-ca`. It directly issues 24-hour certificates for
Manager, Agent, Gateway, and Cache mutual TLS. The root remains stable while
the issuing CA and workload certificates rotate, so normal rotation does not
restart workloads or require another cluster service.

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

This internal CA is separate from browser-facing HTTPS. cert-manager is
optional and can populate the TLS Secret referenced by an Ingress or an
external Gateway listener.

## Install

Create the administration token and install the newest development chart. The
default image is `ghcr.io/explodingdragon/gitea-codespace:main`:

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

Pushes to `main` publish a Helm prerelease named
`0.1.0-main.<run-number>`. For a reproducible installation, use the exact chart
version and image digest from the workflow summary:

```sh
helm upgrade --install codespace \
  oci://ghcr.io/explodingdragon/charts/gitea-codespace \
  --version 0.1.0-main.REPLACE_WITH_RUN_NUMBER \
  --namespace codespace-system \
  --set image.digest=sha256:REPLACE_WITH_THE_PLATFORM_IMAGE_DIGEST \
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

To publish the page, enable `admin.ingress`, set an HTTPS `admin.publicURL`,
and provide `admin.ingress.host` and `admin.ingress.tlsSecret`. The Secret can
be created by cert-manager, a cloud certificate controller, or the cluster
operator. The chart only references it and does not couple public TLS to the
internal component CA.

## Gateway API

Configure the parent Gateway and listener names on each Gateway component in
the administration page. Manager verifies that the HTTP listener uses HTTP or
HTTPS and that the optional SSH listener uses TCP, then maintains that
component's HTTPRoute and TCPRoute. Per-component selection allows separate
Gateway components to use different public domains and listeners without
changing the Helm release.

HTTPS certificates for Codespace endpoint hostnames belong to the external
Gateway listener and its `certificateRefs`. The cluster operator can use
cert-manager to maintain that Secret. SSH uses the Gateway SSH host key and is
not part of the X.509 certificate hierarchy.

Leaving the parent name empty keeps public route creation under the cluster
operator's control. The component Service remains available inside the
cluster for an operator-managed Ingress, Gateway route, or port-forward.

## Internal CA recovery

Back up `Secret/<release>-internal-ca` with the other management resources.
Manager reports an explicit startup error when the Secret is missing or
invalid while managed state exists. Restore the original Secret and restart
Manager; Agent, Gateway, and Cache will reconnect using the same root.

Creating a replacement CA is appropriate only for an empty installation. This
distinction prevents a deleted Secret from turning into an undetected second
trust domain.

## Configuration and validation

[`values.yaml`](values.yaml) contains defaults. [`values.schema.json`](values.schema.json)
is the authoritative list of supported values and validation rules. Render and
validate the chart through the repository Makefile:

```sh
make helm-check
```
