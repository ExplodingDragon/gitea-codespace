# Gitea Codespace

Gitea Codespace runs isolated, Dev Container-based development environments on Kubernetes. This repository contains the Manager, Runtime Agent, Gateway, Cache, administration UI, Helm chart, and reusable [`devcontainer`](devcontainer) Go package.

Gitea owns users, repository permissions, and lifecycle intent. The Manager owns Kubernetes resources and runtime execution. Runtime Pods run a dedicated Docker daemon with an explicitly selected Kata Containers or Sysbox RuntimeClass; they receive neither a Kubernetes API token nor the node container-runtime socket. The complete architecture and lifecycle are maintained in the [design repository](https://github.com/ExplodingDragon/gitea-dev).

## Build

Use the Go version declared in [`go.mod`](go.mod), pnpm, and a container tool compatible with the `docker build` command.

```sh
make install-proto
make format
make lint
make test
make build
```

`make build` embeds the Vue administration UI in `bin/gitea-codespace`. `make images` cross-compiles the `linux/amd64` binary on the host and then builds the single platform image used by Manager, Agent, Gateway, and Cache. The Docker build only packages that binary and its runtime dependencies. Internal Agent and component protocols live under `internal/rpc`; `make proto-generate` updates their checked-in Go bindings.

Pull requests run static checks, backend and browser tests, and a platform-image smoke test. Pushes to `main` publish `ghcr.io/explodingdragon/gitea-codespace:main` and a Helm prerelease named `0.1.0-main.<run-number>`. The workflow summary records the exact chart version and immutable digests.

## Deploy

Use the [`gitea-codespace` Helm chart](charts/gitea-codespace). The target cluster must provide cert-manager, an internal CA-backed `ClusterIssuer`, and a verified RuntimeClass/StorageClass pair. The shared issuer gives Manager, Agent, Gateway, and Cache certificates from one trust chain, which is required for their mutual TLS connections.

For a development cluster using the standard `cert-manager` namespace, create the example CA and wait until its issuer is ready:

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

The example issuer is a development bootstrap. Production clusters should use their existing internal CA issuer and set `identity.clusterIssuer` during installation. Create the administration token, then install the newest development chart. Its default platform image is the continuously updated `main` tag:

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

For a reproducible installation, copy the chart version and image digest from a successful workflow summary, add `--version 0.1.0-main.<run-number>`, and set `image.digest`. The chart digest can also replace the chart version in the OCI reference. Tagged images are always checked for updates when a Pod is created; digest-pinned images use the local copy when present. Existing Runtime Pods are not restarted merely because `main` changes. Both GHCR packages must be public unless pull credentials are configured.

The chart creates a cert-manager `Certificate`; cert-manager then creates the identity Secret mounted by the Manager. Verify both before opening the administration page:

```sh
kubectl -n codespace-system wait \
  --for=condition=Ready certificate \
  --selector=app.kubernetes.io/instance=codespace \
  --timeout=120s
kubectl -n codespace-system get pods
```

The administration Service defaults to `ClusterIP`:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

The chart [installation guide](charts/gitea-codespace/README.md) covers production issuer requirements and certificate troubleshooting.

For local development against the current kubeconfig, build the binary and inspect the supported Manager flags:

```sh
make build
./bin/gitea-codespace serve --help
```

## Test

- `make test` runs the complete non-E2E Go suite.
- `make test-smoke` runs the short backend regression suite.
- `make frontend-check` and `pnpm --dir web test` validate the administration UI.
- `make helm-check` validates the chart and rendered resources.
- `make test-e2e` runs the real Kubernetes product flow.

Use a dedicated test cluster for E2E. Each run installs its own Manager control plane; cluster-scoped GiteaSite and EnvironmentTemplate resources are coordinated by that control plane, so a separate namespace alone does not isolate another Manager installation.

The E2E target requires a digest-pinned `CODESPACE_IMAGE`, the selected `KUBERNETES_RUNTIME_ISOLATION`, `KUBERNETES_RUNTIME_CLASS`, and `KUBERNETES_STORAGE_CLASS`, plus a reachable Gitea Manager credential through the `CODESPACE_E2E_GITEA_*` variables. It validates one isolation mode per run; execute it separately for each supported Kata or Sysbox environment. Successful runs clean their namespace, while failed runs retain diagnostics before cleanup.

## License

[MIT](LICENSE).
