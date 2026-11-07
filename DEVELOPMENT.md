# Developing Gitea Codespace

This guide covers repository setup, local validation, generated sources, and
Kubernetes end-to-end testing. Use the [Helm guide](charts/gitea-codespace/README.md)
for deployment and the
[architecture documents](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)
for protocol and controller design.

## Set up

- The Go version declared in [`go.mod`](go.mod)
- Node.js and pnpm
- Helm and kubectl for Kubernetes work
- A Docker-compatible tool for building the platform image
- curl, xz, and sha256sum for downloading and verifying the bundled Gitea CLI

Install the pinned Go development tools and frontend dependencies:

```sh
make install-tools
pnpm --dir web install --frozen-lockfile
pnpm --dir web exec playwright install chromium
```

Pinned tool versions keep local generation and validation consistent with CI.

## Repository layout

| Path | Responsibility |
| --- | --- |
| `cmd` | Process entry points and runtime tools |
| `devcontainer` | Dev Container parsing, merging, execution, and specification tests |
| `internal` | Manager, Agent, Gateway, Cache, controllers, and shared runtime packages |
| `web` | Vue administration interface and Playwright tests |
| `charts`, `deploy` | Helm chart, CRDs, and platform image |
| `tests/e2e` | Kubernetes lifecycle test harness |

The Dev Container model preserves specification-defined fields throughout
parsing, merging, and resolution so repository configurations remain portable.

## Work locally

Use the Makefile so local commands use the same options as CI.

| Command | Purpose |
| --- | --- |
| `make format` | Format Go, shell, protobuf, and frontend sources |
| `make lint` | Check generated files, scripts, workflows, TypeScript, Helm, CRDs, and Go |
| `make test` | Run the standard Go test suite |
| `make test-frontend` | Run administration interface tests |
| `make build` | Build the frontend and local binary |
| `make clean` | Remove local build and test output |

Run focused package tests while iterating, then run `make lint`, `make test`,
and `make test-frontend` before submitting a change.

## Generated files

Generated Kubernetes and RPC sources are committed so builds and chart installs
do not require generators. Regenerate them after changing a source definition.

Regenerate Kubernetes APIs with:

```sh
make generate-kubernetes
make kubernetes-check
```

This updates the canonical CRDs and their Helm copies.

Regenerate internal Agent and component RPC bindings with:

```sh
make proto-format
make proto-generate
make proto-check
```

The public Gitea-to-Manager protocol lives in `gitea.dev/codespace-proto-go`.
Publish that module before updating its dependency here.

## Administration interface

Start the frontend development server with:

```sh
make admin-dev
```

Vite proxies `/api` to `http://127.0.0.1:18080`. Run Manager locally or
port-forward its administration Service. Add user-visible text to the JSON
catalogs in `web/locales`.

## Platform image

Build the platform image with:

```sh
make images
```

The build downloads the latest stable Gitea CLI for `linux/amd64`, verifies the
official checksum and architecture, and packages it with the platform binary.
Set `GITEA_CLI_VERSION` when reproducing an image with a specific CLI release.
Set `CODESPACE_IMAGE` or `CONTAINER_TOOL` to use another image name or builder.

## Kubernetes end-to-end tests

The E2E suite verifies the complete lifecycle in a real cluster. It requires a
Kata or Sysbox RuntimeClass, a matching StorageClass, a reachable Gitea test
endpoint, and an immutable platform image.

Use an isolated test cluster:

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

The harness isolates the run, collects failure diagnostics, and removes its
resources on exit. CI covers Kata and Sysbox. Local development normally uses
the standard test suite.
