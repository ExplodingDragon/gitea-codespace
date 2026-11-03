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

Pull requests run static checks, backend and browser tests, a platform-image smoke test, and the complete Sysbox and Kata E2E suites for trusted branches. Pushes to `main` publish `ghcr.io/explodingdragon/gitea-codespace:main` and a Helm prerelease named `0.1.0-main.<run-number>` only after both runtime suites pass. The workflow summary records the exact chart version and immutable digests.

## Deploy

Use the [`gitea-codespace` Helm chart](charts/gitea-codespace). The target cluster must provide a verified RuntimeClass/StorageClass pair. Manager initializes an installation-specific internal CA and directly issues short-lived certificates for Manager, Agent, Gateway, and Cache mutual TLS. This trust hierarchy is independent from public HTTPS and does not require cert-manager.

Create the administration token, then install the newest development chart. Its default platform image is the continuously updated `main` tag:

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

Manager creates `<release>-internal-ca` in the management namespace before its internal listeners become ready. Treat this Secret as installation state: enable Kubernetes Secret encryption at rest and include it in backups. Verify the control plane before opening the administration page:

```sh
kubectl -n codespace-system rollout status \
  deployment/codespace-gitea-codespace \
  --timeout=120s
kubectl -n codespace-system get secret codespace-gitea-codespace-internal-ca
```

The administration Service defaults to `ClusterIP`:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

The chart [installation guide](charts/gitea-codespace/README.md) covers internal CA recovery and optional public HTTPS.

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
- `make test-e2e` runs the real Kubernetes product flow against the explicitly selected kubeconfig.

Use a dedicated, single-node, direct k3s cluster for E2E and set `KUBECONFIG` to its readable configuration file. The explicit kubeconfig keeps ordinary local tests independent from Kubernetes and prevents an accidental run against the developer's current default context. Each run installs its own Manager control plane; cluster-scoped GiteaSite and EnvironmentTemplate resources are coordinated by that control plane, so a separate namespace alone does not isolate another Manager installation.

The E2E target requires a digest-pinned `CODESPACE_IMAGE`, the selected `KUBERNETES_RUNTIME_ISOLATION`, `KUBERNETES_RUNTIME_CLASS`, and `KUBERNETES_STORAGE_CLASS`, plus a reachable Gitea Manager credential through the `CODESPACE_E2E_GITEA_*` variables. It validates one isolation mode per run; execute it separately for each supported Kata or Sysbox environment. Successful runs clean their namespace, while failed runs retain diagnostics before cleanup.

When the test cluster needs an HTTP proxy to pull public Dev Container images, set `CODESPACE_E2E_HTTP_PROXY`. The runner injects it only into the disposable Docker Runtime Pod, keeping the product configuration and the cluster control plane unchanged.

CI creates a single-node k3s cluster on a fresh GitHub-hosted `ubuntu-24.04` runner for each isolation mode. The Sysbox job installs its verified release package and uses the built-in local-path StorageClass. The Kata job requires the runner's KVM device, installs the official Kata deployment chart, and uses RawFile LocalPV for dynamically provisioned block volumes. Both jobs import the platform image built by the hosted build job and invoke the same `make test-e2e` entry point used for an explicit developer run. Repository variables provide `CODESPACE_E2E_GITEA_URL` and `CODESPACE_E2E_GITEA_MANAGER_ID`; `CODESPACE_E2E_GITEA_MANAGER_SECRET` is a repository secret. Pull requests from forks keep the standard checks but skip the credentialed E2E jobs because GitHub does not expose repository secrets to them.

## License

[MIT](LICENSE).
