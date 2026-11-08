# Developing Gitea Codespace

This guide covers the contributor workflow. Use the
[Helm guide](charts/gitea-codespace/README.md) for cluster operations and the
[architecture documents](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)
for design rationale.

## Set up

Use the Go version declared in [`go.mod`](go.mod), Node.js, and pnpm. Kubernetes
work also needs Helm and kubectl; image builds need a Docker-compatible tool,
curl, xz, and sha256sum.

```sh
make install-tools
pnpm --dir web install --frozen-lockfile
pnpm --dir web exec playwright install chromium
```

The Makefile pins development tools so local checks use the same versions as
CI.

## Repository layout

| Path               | Responsibility                                                     |
| ------------------ | ------------------------------------------------------------------ |
| `cmd`              | Process entry points                                                |
| `devcontainer`     | Dev Container specification parsing and execution                  |
| `internal`         | Manager, Agent, Gateway, Cache, and shared platform implementation |
| `web`              | Vue administration interface                                       |
| `charts`, `deploy` | Helm resources, CRDs, and platform image                           |
| `tests/e2e`        | Kubernetes lifecycle test harness                                  |

Dev Container specification fields remain in `devcontainer`; platform behavior
belongs in `internal`. This boundary keeps repository configuration portable to
other compliant Dev Container implementations.

## Validate changes

Use the public Make targets because CI runs the same commands.

| Command              | Purpose                                                                   |
| -------------------- | ------------------------------------------------------------------------- |
| `make format`        | Format Go, shell, protobuf, and frontend sources                          |
| `make lint`          | Check generated files, scripts, workflows, TypeScript, Helm, CRDs, and Go |
| `make test`          | Run standard Go tests                                                     |
| `make test-frontend` | Run administration interface tests                                       |
| `make build`         | Build the frontend and local binary                                       |
| `make clean`         | Remove generated local output                                             |

Run focused package tests while editing. Before submitting a change, run
`make lint`, `make test`, and `make test-frontend`.

## Generated sources

Generated Kubernetes and RPC sources are committed so normal builds remain
independent of the generator toolchain.

| Source changed                    | Command                                        |
| --------------------------------- | ---------------------------------------------- |
| Kubernetes API types              | `make generate-kubernetes kubernetes-check`    |
| Internal Agent or component Proto | `make proto-format proto-generate proto-check` |

The public Gitea-to-Manager protocol lives in `gitea.dev/codespace-proto-go`.
Publish that module before updating this repository so consumers resolve a
reproducible dependency.

## Run development services

`make admin-dev` starts the administration frontend and proxies `/api` to
`http://127.0.0.1:18080`. Run Manager locally or port-forward its administration
Service. Add user-visible text to every JSON catalog under `web/locales`.

`make images` builds the platform image and packages the verified Gitea CLI.
`CODESPACE_IMAGE` selects the image name and `CONTAINER_TOOL` selects the
builder.

## Run Kubernetes E2E

The E2E suite needs a test cluster, matching runtime and storage support, a
Gitea endpoint reachable from the cluster, and an immutable platform image.
Kata and Sysbox use labeled RuntimeClasses; Native uses the node's default CRI
runtime and exercises Device Plugin/CDI propagation.

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

The harness records diagnostics on failure and removes its temporary cluster
resources on exit. CI runs the Kata, Sysbox, and Native variants.
