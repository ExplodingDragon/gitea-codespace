# Developing Gitea Codespace

This guide covers the supported local development workflow. Deployment and
cluster operation belong in the [Helm guide](charts/gitea-codespace/README.md),
while architectural decisions are maintained in the
[design repository](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace).

## Prerequisites

- the Go version declared in [`go.mod`](go.mod);
- Node.js and pnpm for the administration interface;
- Helm and kubectl for chart or Kubernetes changes; and
- Docker or another compatible container tool for building the platform image.

Install the pinned Go development tools and frontend dependencies:

```sh
make install-tools
pnpm --dir web install --frozen-lockfile
pnpm --dir web exec playwright install chromium
```

The Makefile pins the Go tools used by CI, so local generation and validation
produce the same results.

## Repository layout

| Path | Responsibility |
| --- | --- |
| `cmd` | CLI entry points for Manager, Agent, Gateway, Cache, and runtime tools |
| `devcontainer` | Dev Container parsing, merging, Docker execution, and interoperability tests |
| `internal` | Runtime services, Kubernetes controllers, protocols, and shared implementation |
| `web` | Vue administration interface and Playwright tests |
| `charts` and `deploy` | Helm package, CRDs, and platform image |
| `tests/e2e` | Real-cluster lifecycle test harness |

The public Dev Container model preserves specification-defined fields even
when a field is not yet consumed by the runtime. This keeps repository
configurations portable and prevents parsing or merge stages from silently
discarding standard data.

## Development workflow

Use the Makefile as the development entry point:

```sh
make format
make lint
make test
make test-frontend
make build
```

- `make format` formats Go, shell, protobuf, and frontend sources.
- `make lint` checks generated protobuf and CRD output, shell and workflow
  syntax, TypeScript and frontend formatting, Helm rendering, `go vet`, and
  staticcheck.
- `make test` runs the standard non-E2E Go suite serially.
- `make test-frontend` runs the administration workflow tests with Playwright.
- `make build` builds the frontend and the local `gitea-codespace` binary.

Run focused package tests while iterating, then use the complete commands above
before submitting. `make clean` removes local build and test output.

## Generated sources

Kubernetes and protobuf outputs are checked in because releases and the Helm
chart consume them directly. Update their sources first, then regenerate the
derived files.

Regenerate Kubernetes APIs with:

```sh
make generate-kubernetes
make kubernetes-check
```

The command writes the canonical CRDs to `deploy/crds` and copies them into the
Helm chart because both release paths consume the same schema.

Regenerate internal Agent or component protocols with:

```sh
make proto-format
make proto-generate
make proto-check
```

The public Gitea-to-Manager protocol belongs to the separate
`gitea.dev/codespace-proto-go` module so Gitea does not depend on Manager
implementation packages. Publish that module before updating this dependency.

## Focused workflows

### Administration interface

Start the frontend development server with:

```sh
make admin-dev
```

Vite proxies `/api` to `http://127.0.0.1:18080`; run a local Manager or
port-forward its administration Service when testing API-backed workflows.
User-visible text belongs in the shared JSON locale catalogs under
`web/locales`.

### Platform image

Build the Linux binary before packaging the platform image. Keeping the Go
build outside the container build makes local and CI artifacts follow the same
path:

```sh
make build-linux
make images
```

Override `CODESPACE_IMAGE` or `CONTAINER_TOOL` for another image name or
container builder.

### Kubernetes E2E

The E2E suite exercises the Manager, controllers, Gateway, Cache, Runtime
Agent, persistent Docker state, and Dev Container creation in a real cluster.
It requires a prepared Kata or Sysbox RuntimeClass, a compatible StorageClass,
a reachable Gitea test endpoint, and an immutable platform image reference.

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

The harness creates isolated resources, collects failure diagnostics, and
removes its resources when it exits. CI runs the same entry point for Kata and
Sysbox; ordinary changes use the standard test suite.

## Before submitting

Run `make lint`, `make test`, and `make test-frontend`. Add focused coverage for
changed lifecycle or isolation behavior, update the document used by the
affected audience, and remove temporary test resources. These shared entry
points also verify that checked-in protobuf and CRD output matches its source.
