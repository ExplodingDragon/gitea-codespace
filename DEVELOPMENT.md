# Developing Gitea Codespace

This guide covers source changes and local validation. Use the
[Helm guide](charts/gitea-codespace/README.md) for deployment and the
[architecture documents](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)
for system design.

## Set up

Use the Go version declared in [`go.mod`](go.mod), Node.js, and pnpm. Changes to
Kubernetes resources also need Helm and kubectl. Image builds need a
Docker-compatible container tool and the archive and checksum tools used by the
Makefile.

```sh
make install-tools
pnpm --dir web install --frozen-lockfile
pnpm --dir extension install --frozen-lockfile
pnpm --dir web exec playwright install chromium
```

The Makefile installs pinned development tools so local and CI checks use the
same versions.

## Repository layout

| Path               | Responsibility                                              |
| ------------------ | ----------------------------------------------------------- |
| `cmd`              | Process entry points                                         |
| `devcontainer`     | Dev Container specification parsing and execution           |
| `internal`         | Manager, Agent, Gateway, Cache, and platform implementation |
| `web`              | Administration interface                                    |
| `extension`        | Web IDE extension                                           |
| `charts`, `deploy` | Helm resources, CRDs, and platform image                    |
| `tests/e2e`        | Kubernetes lifecycle tests                                  |

Keep Dev Container specification fields in `devcontainer` and Codespace
platform behavior in `internal`. This keeps repository configuration portable.
The Agent records observed platform results, such as whether the optional Web
IDE started, beside the Dev Container recovery state rather than in
`devcontainer.State`. Requested configuration and the services observed during
a runtime operation therefore remain separate.

## Validate

Use the public Make targets because CI runs the same commands.

| Command              | Purpose                                                                   |
| -------------------- | ------------------------------------------------------------------------- |
| `make format`        | Format Go, shell, protobuf, and frontend sources                          |
| `make lint`          | Check generated files, scripts, workflows, TypeScript, Helm, CRDs, and Go |
| `make test`          | Run standard Go tests                                                     |
| `make test-frontend` | Run administration interface and Web IDE extension tests                 |
| `make build`         | Build the frontend and local binary                                       |
| `make clean`         | Remove generated local output                                             |

Run focused package tests while editing. Before submitting, run `make lint`,
`make test`, and `make test-frontend`.

## Generated sources

Generated Kubernetes and RPC sources are committed so normal builds do not need
the generator toolchain.

| Source changed                    | Command                                        |
| --------------------------------- | ---------------------------------------------- |
| Kubernetes API types              | `make generate-kubernetes kubernetes-check`    |
| Internal Agent or component RPC   | `make proto-format proto-generate proto-check` |

The public Gitea-to-Manager protocol lives in `gitea.dev/codespace-proto-go`.
Release it before updating this repository so every consumer resolves the same
immutable version.

## Local workflows

`make admin-dev` starts the administration frontend and proxies `/api` to
`http://127.0.0.1:18080`. Run Manager locally or port-forward its administration
Service first.

Add administration text to every catalog under `web/locales`. Add extension
text to `extension/package.nls.*.json` or `extension/l10n`.

`make images` prepares the Gitea CLI, code-server distribution, and Codespace
VSIX before building the platform image. Set `CODESPACE_IMAGE` to select the
image name and `CONTAINER_TOOL` to select the builder.

## Run Kubernetes E2E

Use a disposable test cluster for E2E. The suite needs compatible runtime and
storage support, a Gitea endpoint reachable from the cluster, and an immutable
platform image. Kata and Sysbox use a labeled RuntimeClass; Native tests use the
default CRI runtime and the test Device Plugin.

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

The harness records failure diagnostics and removes temporary resources. CI
runs the Kata, Sysbox, and Native variants.
