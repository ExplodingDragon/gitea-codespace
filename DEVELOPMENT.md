# Developing Gitea Codespace

This guide covers the local build, generation, and test workflow. Use the
[Helm guide](charts/gitea-codespace/README.md) for deployment and the
[architecture documents](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)
for protocol and controller design.

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

The Makefile pins the generators and linters used by CI so generated files and
validation results remain reproducible.

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

## Build and test

Use the Makefile as the development entry point:

```sh
make format
make lint
make test
make test-frontend
make build
```

| Command | Purpose |
| --- | --- |
| `make format` | Format Go, shell, protobuf, and frontend sources. |
| `make lint` | Validate generated files, scripts, workflows, TypeScript, Helm, CRDs, Go, and formatting. |
| `make test` | Run the standard non-E2E Go suite. |
| `make test-frontend` | Run the administration workflows with Playwright. |
| `make build` | Build the frontend and local `gitea-codespace` binary. |

Use focused package tests while iterating, then run the complete validation
commands before submitting. `make clean` removes generated local build and test
output.

## Generated sources

Kubernetes and protobuf outputs are checked in because the binary and Helm
chart consume them without running generators. Update the source definition,
then regenerate and validate its derived files.

Regenerate Kubernetes APIs with:

```sh
make generate-kubernetes
make kubernetes-check
```

This writes the canonical CRDs to `deploy/crds` and copies the same schema into
the Helm chart.

Regenerate internal Agent or component protocols with:

```sh
make proto-format
make proto-generate
make proto-check
```

The public Gitea-to-Manager protocol is maintained in
`gitea.dev/codespace-proto-go`. Keeping it in a separate module lets Gitea use
the public contract without importing Manager implementation packages. Publish
the protocol module before updating this repository's dependency.

## Common tasks

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

The E2E suite verifies the complete lifecycle in a real cluster. It requires a
prepared Kata or Sysbox RuntimeClass, a compatible StorageClass, a Gitea test
endpoint reachable from the cluster, and an immutable platform image.

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

The harness labels all resources with a unique run identifier, collects
diagnostics on failure, and removes the isolated test resources when it exits.
CI uses this entry point for both Kata and Sysbox; local development uses the
standard test suite unless a prepared test cluster is available.
