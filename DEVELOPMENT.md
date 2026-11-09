# Developing Gitea Codespace

This guide covers the repository layout, local development, and validation.
Use the [Helm guide](charts/gitea-codespace/README.md) for deployment and the
[architecture documents](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)
for system design decisions.

## Set up

Use the Go version declared in [`go.mod`](go.mod), Node.js, and pnpm. Changes to
Kubernetes resources also need Helm and kubectl. Image builds need a
Docker-compatible container tool.

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

Keep Dev Container specification fields in `devcontainer` and platform behavior
in `internal`. This preserves repository portability while allowing Codespace
to manage Kubernetes and runtime state independently.

## Gateway proxy design

Gateway authenticates a browser before forwarding traffic and binds the session
to one Gitea site, user, codespace, and endpoint. HTTP requests with an `Origin`
must match the complete external Workspace origin. When a normal HTTP request
does not carry `Origin`, `Sec-Fetch-Site: same-origin` provides the browser's
equivalent signal; WebSocket upgrades always require the exact `Origin`. This
keeps sibling subdomains isolated while accepting browser APIs that omit Origin
on a same-origin request.

The validated external Host is forwarded to the provider because editors use it
to construct links, cookies, redirects, and WebSocket URLs. The internal runtime
address remains only the transport target. Gateway rebuilds forwarded identity
headers and keeps provider Service Worker headers, so standard browser features
work without allowing request headers to choose another runtime.

Long-running requests use the client context and per-instance concurrency
limits instead of a fixed response-header deadline. A transient permission
validation error leaves an established connection running and is retried at the
next interval; an explicit denial closes it. A runtime target change closes old
upstream connections but retains the browser session so the browser can connect
to the replacement target. Active connections do not count as idle sessions.

### Implementation acceptance criteria

- Same-origin HTTP APIs, exact-origin WebSockets, Service Workers, slow responses,
  and provider-generated external URLs work through Gateway.
- Cross-origin requests, mismatched site or endpoint sessions, spoofed identity
  headers, and invalid Agent identities remain rejected.
- Provider startup delay, a transient authorization error, or a runtime target
  replacement can recover without reopening the codespace from Gitea.

## Validate

Use the public Make targets because CI runs the same commands.

| Command              | Purpose                                                                   |
| -------------------- | ------------------------------------------------------------------------- |
| `make format`        | Format Go, shell, protobuf, and frontend sources                          |
| `make lint`          | Check generated files, scripts, workflows, TypeScript, Helm, CRDs, and Go |
| `make test`          | Run standard Go tests                                                     |
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
focused provider validation. Each provider's `image` target builds an image for
the current architecture. See the [provider development
guide](providers/DEVELOPMENT.md) for the complete third-party integration
workflow.

## Kubernetes E2E

The E2E suite uses a deterministic test provider and a disposable Kubernetes
cluster. It validates the workspace lifecycle, Gateway access, storage, and the
selected isolation runtime.

```sh
make images e2e-web-ide-image

export KUBECONFIG=/path/to/test-kubeconfig
export CODESPACE_E2E_GITEA_LISTEN=<host-and-port-reachable-from-the-cluster>
export CODESPACE_E2E_NODE_ADDRESS=<worker-address-reachable-from-the-test-process>

make test-e2e \
  KUBERNETES_RUNTIME_ISOLATION=sysbox \
  KUBERNETES_RUNTIME_CLASS=sysbox-runc \
  KUBERNETES_STORAGE_CLASS=<storage-class>
```

The harness imports the locally built images, records failure diagnostics, and
removes its temporary resources. CI runs Kata, Sysbox, and Native variants.
