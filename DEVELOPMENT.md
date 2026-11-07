# Developing Gitea Codespace

This guide covers the repository layout and contributor workflows. Deployment
belongs in the
[Helm guide](charts/gitea-codespace/README.md); the rationale for protocol and
controller decisions belongs in the
[architecture documents](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace).

## Prerequisites

- the Go version declared in [`go.mod`](go.mod);
- Node.js and pnpm;
- Helm and kubectl for Kubernetes work; and
- a Docker-compatible tool for building the platform image.

Install the pinned Go development tools and frontend dependencies:

```sh
make install-tools
pnpm --dir web install --frozen-lockfile
pnpm --dir web exec playwright install chromium
```

The Makefile pins development tools so local generation matches CI.

## Repository layout

| Path | Responsibility |
| --- | --- |
| `cmd` | Process entry points and runtime tools |
| `devcontainer` | Dev Container parsing, merging, execution, and specification tests |
| `internal` | Manager, Agent, Gateway, Cache, controllers, and shared runtime packages |
| `web` | Vue administration interface and Playwright tests |
| `charts`, `deploy` | Helm chart, CRDs, and platform image |
| `tests/e2e` | Kubernetes lifecycle test harness |

The public Dev Container model preserves specification-defined fields even
when a field is not yet consumed by the runtime. This keeps repository
configurations portable and prevents parsing or merge stages from silently
discarding standard data.

## Development workflow

The Makefile is the supported entry point because it applies the same tool
versions and options as CI.

| Command | Purpose |
| --- | --- |
| `make format` | Format Go, shell, protobuf, and frontend sources |
| `make lint` | Check generated files, scripts, workflows, TypeScript, Helm, CRDs, and Go |
| `make test` | Run the standard Go test suite |
| `make test-frontend` | Run administration interface tests |
| `make build` | Build the frontend and local binary |
| `make clean` | Remove local build and test output |

Run focused package tests while iterating, then run the standard lint and test
targets before submitting a change.

## Generated sources

Generated Kubernetes and RPC sources are checked in so ordinary builds and
chart installs do not need generator tools. Regenerate them after changing a
source definition.

Regenerate Kubernetes APIs with:

```sh
make generate-kubernetes
make kubernetes-check
```

This updates the canonical CRDs in `deploy/crds` and their Helm copies.

Regenerate internal Agent and component RPC bindings with:

```sh
make proto-format
make proto-generate
make proto-check
```

The public Gitea-to-Manager protocol lives in `gitea.dev/codespace-proto-go`.
Publish that module before updating this repository so Gitea and Manager can use
the same public contract.

## Targeted workflows

### Administration interface

Start the frontend development server with:

```sh
make admin-dev
```

Vite proxies `/api` to `http://127.0.0.1:18080`. Run Manager locally or
port-forward its administration Service when the page needs API data. Add
user-visible text to the JSON catalogs in `web/locales`.

### Platform image

Build the Linux binary before packaging the image so local and CI builds use the
same artifact path:

```sh
make build-linux
make images
```

Set `CODESPACE_IMAGE` or `CONTAINER_TOOL` to use another image name or builder.

### Kubernetes E2E

The E2E suite verifies the complete lifecycle in a real cluster. It requires a
Kata or Sysbox RuntimeClass, a compatible StorageClass, a Gitea test endpoint
reachable from the cluster, and an immutable platform image.

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
resources on exit. CI covers Kata and Sysbox; local development normally uses
the standard test suite.
