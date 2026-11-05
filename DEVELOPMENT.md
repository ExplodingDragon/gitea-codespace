# Developing Gitea Codespace

This guide covers the supported local development workflow. Deployment and
cluster operation belong in the [Helm guide](charts/gitea-codespace/README.md),
while architectural decisions are maintained in the
[design repository](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace).

## Prerequisites

- the Go version declared in [`go.mod`](go.mod);
- pnpm and a Node.js version supported by the current frontend dependencies;
- Helm and kubectl for chart or Kubernetes work; and
- Docker or another compatible container tool for building the platform image.

Install the pinned Go development tools and frontend dependencies:

```sh
make install-tools
pnpm --dir web install --frozen-lockfile
pnpm --dir web exec playwright install chromium
```

`make install-tools` installs Buf, protobuf generators, shfmt, actionlint, and
staticcheck at the versions used by the Makefile. Using these versions keeps
local checks consistent with CI.

## Repository layout

| Path | Responsibility |
| --- | --- |
| `cmd` | CLI commands for Manager, Agent, Gateway, Cache, and runtime tools |
| `devcontainer` | Dev Container parsing, resolution, Docker execution, and interoperability tests |
| `internal/cluster` | Kubernetes APIs, controllers, administration API, and site coordination |
| `internal/agent` | Runtime Agent lifecycle and workspace operations |
| `internal/gateway` | Authorized HTTP, SSH, and SFTP routing |
| `internal/cache` | OCI mirror and build-cache service |
| `internal/rpc` | Internal Agent and component protocol sources and generated Go bindings |
| `web` | Vue administration interface and Playwright tests |
| `charts/gitea-codespace` | Helm chart and packaged CRDs |
| `deploy` | Container image and standalone CRD artifacts |
| `tests/e2e` | Kubernetes lifecycle test harness |

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

Run focused package tests while iterating. Before submitting, use the complete
commands above; they are the same entry points used by CI. `make clean` removes
binaries, generated frontend output, temporary workspaces, and test reports.

## Generated sources

Kubernetes and protobuf outputs are checked in because releases and the Helm
chart consume them directly. Update their sources first, then regenerate the
derived files.

For Kubernetes API changes:

```sh
make generate-kubernetes
make kubernetes-check
```

`make generate-kubernetes` writes the canonical CRDs to `deploy/crds` and
copies the same files into the Helm chart.

For internal Agent or component protocol changes:

```sh
make proto-format
make proto-generate
make proto-check
```

The public Gitea-to-Manager protocol comes from the separate
`gitea.dev/codespace-proto-go` module. Publish that module first, then update
the dependency in this repository without a local `replace` directive.

## Administration interface

Start the frontend development server with:

```sh
make admin-dev
```

Vite serves the interface on its local development address and proxies `/api`
to `http://127.0.0.1:18080`. Run a local Manager administration server or
port-forward a Manager Service to that address before testing API-backed
workflows. User-visible text belongs in the JSON locale catalogs under
`web/locales`.

## Platform image and E2E

Build the Linux binary before packaging the platform image. Keeping the Go
build outside the container build makes local and CI artifacts follow the same
path:

```sh
make build-linux
make images
```

Override `CODESPACE_IMAGE` or `CONTAINER_TOOL` when using another image name or
Docker-compatible builder.

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
removes its resources when it exits. CI runs this entry point for both Kata
and Sysbox; ordinary local development only needs the standard test suite.

## Before submitting

- generated protobuf and CRD files match their sources;
- Go, TypeScript, shell, workflow, and Helm checks pass through `make lint`;
- `make test` and `make test-frontend` pass;
- lifecycle or isolation changes include focused tests;
- user-facing behavior and operational prerequisites are documented for the
  audience that uses them; and
- temporary binaries, test reports, and cluster resources are removed.
