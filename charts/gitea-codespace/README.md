# Gitea Codespace Helm chart

This chart installs the Codespace Manager control plane: CRDs, RBAC, Manager
Deployment, internal and administration Services, Manager certificate, and
NetworkPolicy. Gateway, Cache, and Runtime workloads are created later from
objects configured through the administration UI.

## Prerequisites

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- cert-manager and a `ClusterIssuer` for workload identities;
- access to the Gitea Codespace platform image;
- an administrator token stored in a Kubernetes Secret;
- at least one verified Kata Containers or Sysbox RuntimeClass and compatible
  StorageClass before an environment template is enabled.

The chart references existing RuntimeClasses and leaves node configuration to
the cluster operator. Install Kata using its
[installation guide](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md),
or install Sysbox on a dedicated node pool using its
[Kubernetes guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md).
Verify the chosen runtime, storage, and platform image with a real Pod first.
The broader isolation, storage, network, and recovery requirements are defined
in the [deployment design](https://github.com/ExplodingDragon/gitea-dev/blob/main/src/codespace/deployment-requirements.md).

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
  --set admin.tokenSecret.name=codespace-admin
```

Pushes to `main` update the platform image's `main` tag and publish a Helm
prerelease named `0.1.0-main.<run-number>`. `--devel` selects the newest
prerelease. For a reproducible installation, copy the exact chart version and
image digest from the workflow summary:

```sh
helm upgrade --install codespace \
  oci://ghcr.io/explodingdragon/charts/gitea-codespace \
  --version 0.1.0-main.REPLACE_WITH_RUN_NUMBER \
  --namespace codespace-system \
  --set image.digest=sha256:REPLACE_WITH_THE_PLATFORM_IMAGE_DIGEST \
  --set admin.tokenSecret.name=codespace-admin
```

The chart digest shown in the summary can also be appended to the OCI chart
reference. Tagged platform images use Kubernetes `Always` pull policy, while
digest-pinned images use `IfNotPresent`. This keeps the development channel
current and makes reviewed deployments reproducible. After the first
publication, the repository owner must make both GHCR packages public unless
registry credentials are configured.

The selected image reference is shared by Manager and all workloads it creates.
For a private registry, list existing pull Secret names in
`image.pullSecrets`. Manager synchronizes only those credentials to managed
site namespaces.

## Administration access

The administration Service defaults to `ClusterIP`. Open a local session with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Then open <http://127.0.0.1:18080>. To publish the page, enable
`admin.ingress`, set an HTTPS `admin.publicURL`, and provide the host and TLS
Secret. Restrict direct Service access with `networkPolicy.adminIngressCIDRs`
when cluster networking requires additional sources.

## Gateway API

Set `gatewayAPI.parent.name` to an existing Gateway when the Manager should
create managed HTTPRoute and TCPRoute resources for Gateway workloads. The
parent namespace defaults to the chart namespace. Listener names are selected
with `gatewayAPI.parent.httpListener` and `gatewayAPI.parent.sshListener`.

Leaving the parent name empty keeps public route creation under the cluster
operator's control. This is useful when the ingress implementation does not
support TCPRoute or uses an external provisioning workflow.

## Configuration and validation

[`values.yaml`](values.yaml) contains defaults. [`values.schema.json`](values.schema.json)
is the authoritative list of supported values and validation rules. Render and
validate the chart through the repository Makefile:

```sh
make helm-check
```
