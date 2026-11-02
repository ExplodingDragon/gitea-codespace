# Gitea Codespace Helm chart

This chart installs the Codespace Manager control plane: CRDs, RBAC, Manager
Deployment, internal and administration Services, Manager certificate, and
NetworkPolicy. Gateway, Cache, and Runtime workloads are created later from
objects configured through the administration UI.

## Prerequisites

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- cert-manager and a ready, internal CA-backed `ClusterIssuer` for workload
  identities;
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

## Workload identity

Manager, Agent, Gateway, and Cache authenticate each other with certificates
from one trust chain. The chart creates the Manager `Certificate`, and Manager
creates certificates for the workloads it controls. cert-manager materializes
each certificate as a Secret containing `tls.crt`, `tls.key`, and `ca.crt`.
Pods mount these Secrets, so they become Ready only after certificate issuance
succeeds.

`identity.clusterIssuer` selects the issuer and defaults to
`codespace-identity`. A production issuer must be Ready, support URI subject
alternative names and both client and server authentication, and place the CA
chain in `ca.crt`. Keeping the CA outside the chart lets the cluster operator
control its storage, trust scope, and rotation independently of an application
release.

For a development cluster using the standard `cert-manager` namespace, the
repository provides a self-signed root and CA issuer:

```sh
kubectl -n cert-manager wait \
  --for=condition=Available deployment/cert-manager-webhook \
  --timeout=120s

kubectl apply -f deploy/examples/identity.yaml
kubectl -n cert-manager wait \
  --for=condition=Ready certificate/codespace-root \
  --timeout=120s
kubectl wait \
  --for=condition=Ready clusterissuer/codespace-identity \
  --timeout=120s
```

The example CA is intended for development. Select an existing production
issuer with `--set identity.clusterIssuer=ISSUER_NAME`.

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
  --set admin.tokenSecret.name=codespace-admin \
  --wait \
  --timeout 5m
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

## Verify the installation

The default release creates `Certificate/codespace-gitea-codespace` and asks
cert-manager to produce `Secret/codespace-gitea-codespace-identity`. Verify the
issuer, certificate, Secret, and Manager in that order:

```sh
kubectl get clusterissuer codespace-identity
kubectl -n codespace-system get certificate,certificaterequest
kubectl -n codespace-system wait \
  --for=condition=Ready certificate \
  --selector=app.kubernetes.io/instance=codespace \
  --timeout=120s
kubectl -n codespace-system get secret,pod
kubectl -n codespace-system rollout status \
  deployment/codespace-gitea-codespace \
  --timeout=120s
```

Release name, namespace, and fullname overrides change the generated resource
names. The chart labels remain available for selecting the corresponding
Certificate and workload.

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

## Certificate troubleshooting

An event such as:

```text
MountVolume.SetUp failed ... secret "...-identity" not found
```

means the Pod is waiting for cert-manager to create the Secret named by
`Certificate.spec.secretName`. Inspect the certificate chain from the issuer
downward:

```sh
kubectl get clusterissuer
kubectl -n codespace-system describe certificate
kubectl -n codespace-system get certificaterequest
kubectl -n cert-manager logs deployment/cert-manager --tail=100
```

Once the issuer and Certificate become Ready, cert-manager creates the Secret
and kubelet mounts it without reinstalling the chart. Letting cert-manager own
this Secret preserves the CA chain and certificate rotation expected by all
Codespace components.

## Configuration and validation

[`values.yaml`](values.yaml) contains defaults. [`values.schema.json`](values.schema.json)
is the authoritative list of supported values and validation rules. Render and
validate the chart through the repository Makefile:

```sh
make helm-check
```
