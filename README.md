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

`make build` embeds the Vue administration UI in `bin/gitea-codespace`. `make images` builds the single platform image used by Manager, Agent, Gateway, and Cache. Internal Agent and component protocols live under `internal/rpc`; `make proto-generate` updates their checked-in Go bindings.

## Deploy

Use the [`gitea-codespace` Helm chart](charts/gitea-codespace) with a digest-pinned platform image. The target cluster must already provide cert-manager and a verified RuntimeClass/StorageClass pair. Runtime environments, Gitea sites, Gateways, and Caches are then configured through the administration UI.

The administration Service defaults to `ClusterIP`:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

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

The E2E target requires a digest-pinned `CODESPACE_IMAGE`, the selected `KUBERNETES_RUNTIME_ISOLATION`, `KUBERNETES_RUNTIME_CLASS`, and `KUBERNETES_STORAGE_CLASS`, plus a reachable Gitea Manager credential through the `CODESPACE_E2E_GITEA_*` variables. It validates one isolation mode per run; execute it separately for each supported Kata or Sysbox environment. Successful runs clean their namespace, while failed runs retain diagnostics before cleanup.

## License

[MIT](LICENSE).
