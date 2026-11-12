# Developing Gitea Codespace

This guide covers the repository layout, local development, and validation.
Use the [Helm guide](charts/gitea-codespace/README.md) for deployment and the
[architecture documents](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)
for system design decisions.

## Set up

Use the Go version declared in [`go.mod`](go.mod), Node.js, and pnpm. Kubernetes
resource generation and validation also need Helm; deployment and E2E workflows
need `kubectl`. Image builds need a Docker-compatible container tool.

```sh
make install-tools
pnpm --dir web install --frozen-lockfile
pnpm --dir web exec playwright install chromium
```

The Makefile installs pinned development tools so local and CI checks use the
same versions.

## Repository layout

| Path               | Responsibility                                              |
| ------------------ | ----------------------------------------------------------- |
| `cmd`              | Process entry points                                        |
| `devcontainer`     | Dev Container specification parsing and execution           |
| `internal`         | Manager, Agent, Gateway, Cache, and platform implementation |
| `web`              | Administration interface                                    |
| `providers`        | Independently built official Web IDE images                 |
| `charts`, `deploy` | Helm resources, CRDs, and platform image                    |
| `tests/e2e`        | Kubernetes lifecycle tests and the fake Web IDE image       |

See the [implementation design](https://github.com/ExplodingDragon/gitea-dev/blob/main/src/codespace/implementation.md)
for code ownership and interface boundaries.

## Validate

Use the public Make targets because CI runs the same commands.

| Command              | Purpose                                                                   |
| -------------------- | ------------------------------------------------------------------------- |
| `make format`        | Format Go, shell, protobuf, and frontend sources                          |
| `make lint`          | Check generated files, scripts, workflows, TypeScript, Helm, CRDs, and Go |
| `make test`          | Run all non-E2E Go tests                                                   |
| `make test-frontend` | Run administration interface tests                                        |
| `make build`         | Build the frontend and local binary                                       |
| `make clean`         | Remove generated local output                                             |

Run focused package tests while editing. Before submitting, run `make lint`,
`make test`, and `make test-frontend`.

## Generated sources

Generated Kubernetes and RPC sources are committed so normal builds do not need
the generator toolchain.

| Source changed                  | Command                                        |
| ------------------------------- | ---------------------------------------------- |
| Kubernetes API types            | `make generate-kubernetes kubernetes-check`    |
| Internal Agent or component RPC | `make proto-format proto-generate proto-check` |

The public Gitea-to-Manager protocol lives in `gitea.dev/codespace-proto-go`.
Release it before updating this repository so every consumer resolves the same
immutable version.

## Local workflows

`make admin-dev` starts the administration frontend and proxies `/api` to
`http://127.0.0.1:18080`. Run Manager locally or port-forward its administration
Service first. Add user-visible administration text to every catalog under
`web/locales`.

`make images` builds the platform image. Web IDEs have independent images so an
editor release can be updated without rebuilding the platform services.

Use `make -C providers/vscode check` or `make -C providers/dsh check` for
focused provider validation. The DSH check fetches its pinned upstream sources
and bundles the Codespace plugin against them. Each provider's `image` target
builds an image for the current architecture. See the [provider development
guide](providers/DEVELOPMENT.md) for the third-party integration contract and
image verification steps.

The [VS Code](.github/workflows/provider-vscode.yml) and
[DSH](.github/workflows/provider-dsh.yml) workflows validate their own build
inputs and publish amd64/arm64 images on matching pushes to `main`. Shared
provider inputs trigger both workflows; the [platform workflow](.github/workflows/ci.yml)
handles core changes separately. Workflow path filters define the exact scope.

## Kubernetes E2E

CI runs the workspace lifecycle, Gateway, storage, and device integration tests
on dedicated k3s workers for Kata, Sysbox, and Native runtimes. Ordinary local
development uses the non-E2E commands above.

For an explicit cluster run, follow the [E2E guide](tests/e2e/README.md). It
covers worker preparation, image import, runtime selection, and diagnostics.
The harness performs local worker operations as well as Kubernetes API calls,
so choosing a kubeconfig alone is not enough to prepare a remote cluster.
