# Developing Gitea Codespace

This guide covers the contributor workflow. Use the
[Helm guide](charts/gitea-codespace/README.md) for cluster operations and the
[architecture documents](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)
for design rationale.

## Set up

Local development requires the Go version declared in [`go.mod`](go.mod),
Node.js, and pnpm. Helm and kubectl are needed for Kubernetes work. Image builds
also require a Docker-compatible tool, curl, xz, and sha256sum.

Install the pinned Go development tools and frontend dependencies:

```sh
make install-tools
pnpm --dir web install --frozen-lockfile
pnpm --dir web exec playwright install chromium
```

The Makefile pins code-generation and validation tools so local results match
CI.

## Repository layout

| Path               | Responsibility                                                           |
| ------------------ | ------------------------------------------------------------------------ |
| `cmd`              | Process entry points and runtime tools                                   |
| `devcontainer`     | Dev Container parsing, merging, execution, and specification tests       |
| `internal`         | Manager, Agent, Gateway, Cache, controllers, and shared runtime packages |
| `web`              | Vue administration interface and Playwright tests                        |
| `charts`, `deploy` | Helm chart, CRDs, and platform image                                     |
| `tests/e2e`        | Kubernetes lifecycle test harness                                        |

The `devcontainer` package keeps specification-defined fields through parsing,
merging, and resolution. Codespace-specific behavior belongs in `internal` so
standard repository configurations remain portable.

## Work locally

Use the Makefile because CI invokes the same entry points.

| Command              | Purpose                                                                   |
| -------------------- | ------------------------------------------------------------------------- |
| `make format`        | Format Go, shell, protobuf, and frontend sources                          |
| `make lint`          | Check generated files, scripts, workflows, TypeScript, Helm, CRDs, and Go |
| `make test`          | Run the standard Go test suite                                            |
| `make test-frontend` | Run administration interface tests                                        |
| `make build`         | Build the frontend and local binary                                       |
| `make clean`         | Remove local build and test output                                        |

Run the affected package tests while iterating. Before submitting a change, run
`make lint`, `make test`, and `make test-frontend`.

## Generated files

Generated Kubernetes and RPC sources are committed so normal builds do not need
the generator toolchain. Regenerate the corresponding output whenever its
source definition changes.

| Source changed                    | Commands                                       | Output                  |
| --------------------------------- | ---------------------------------------------- | ----------------------- |
| Kubernetes API types              | `make generate-kubernetes kubernetes-check`    | Canonical and Helm CRDs |
| Internal Agent or component Proto | `make proto-format proto-generate proto-check` | Internal Go bindings    |

The public Gitea-to-Manager protocol is maintained in
`gitea.dev/codespace-proto-go`. Publish that module before updating this
repository so builds use a reproducible remote dependency.

## Administration interface

```sh
make admin-dev
```

Vite proxies `/api` to `http://127.0.0.1:18080`. Run Manager locally or
port-forward its administration Service. User-visible text belongs in the JSON
catalogs under `web/locales` so the interface remains translatable.

## Platform image

```sh
make images
```

The build verifies and packages the platform binary and Gitea CLI. Use
`GITEA_CLI_VERSION` to reproduce a specific CLI release, and use
`CODESPACE_IMAGE` or `CONTAINER_TOOL` to select the output image or builder.

## Kubernetes end-to-end tests

The E2E suite verifies the complete lifecycle in an isolated cluster. It needs a
Kata or Sysbox RuntimeClass, matching storage, a reachable Gitea test endpoint,
and an immutable platform image.

```sh
export KUBECONFIG=/path/to/test-kubeconfig
export CODESPACE_E2E_GITEA_LISTEN=<host-and-port-reachable-from-the-cluster>
export CODESPACE_E2E_NODE_ADDRESS=<worker-address-reachable-from-the-test-process>

make test-e2e \
  CODESPACE_IMAGE='registry.example/gitea-codespace@sha256:<digest>' \
  KUBERNETES_RUNTIME_ISOLATION=sysbox \
  KUBERNETES_RUNTIME_CLASS=sysbox-runc \
  KUBERNETES_STORAGE_CLASS=<storage-class>
```

The harness collects failure diagnostics and removes its resources on exit. CI
runs both supported isolation modes; the standard test suite is sufficient for
changes that do not require a real RuntimeClass or storage driver.
