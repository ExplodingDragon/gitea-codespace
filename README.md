# Gitea Codespace

Gitea Codespace runs isolated, Dev Container-based development environments on
Kubernetes. This repository contains the Manager, Runtime Agent, Gateway,
Cache, administration UI, Helm chart, and reusable [`devcontainer`](devcontainer)
Go package.

Gitea owns users, repository permissions, and lifecycle intent. The Manager
owns Kubernetes resources and runtime execution. Runtime Pods run a dedicated
Docker daemon under a verified Kata Containers or Sysbox RuntimeClass; they do
not receive a Kubernetes API token or the node container-runtime socket.

The [design repository](https://github.com/ExplodingDragon/gitea-dev) documents
the architecture, lifecycle, security boundaries, and deployment requirements.

## Build

The build requires the Go version declared in [`go.mod`](go.mod) and pnpm.

```sh
make build
make generate-kubernetes
```

`make build` compiles and embeds the Vue administration UI. Build the single
platform image used by Manager, Agent, Gateway, and Cache with:

```sh
make images
```

## Deploy

The supported deployment source is the
[`gitea-codespace` Helm chart](charts/gitea-codespace). The target cluster must
provide cert-manager, the selected `ClusterIssuer`, and a verified
RuntimeClass/StorageClass pair. Deploy the published platform image by digest.

The administration Service defaults to `ClusterIP`. After installation, open
the UI locally without publishing another cluster endpoint:

```sh
kubectl -n codespace-system port-forward service/codespace-gitea-codespace-admin 18080:18080
```

Use the administration UI to configure Gitea sites, environment templates,
Gateways, and Caches. Secret values are write-only. Template changes apply to
new environments; existing Codespaces resume with their stored runtime input.

## Local development

Run a Manager against the Kubernetes context in the default kubeconfig:

```sh
./bin/gitea-codespace serve \
  --admin-token-file /path/to/admin-token \
  --certificate-directory /path/to/projected/identity \
  --platform-image registry.example.com/gitea/codespace@sha256:IMAGE_DIGEST
```

The identity directory contains `tls.crt`, `tls.key`, and `ca.crt`. The local
administration URL is <http://127.0.0.1:18080>. Use HTTPS and an explicit
`--admin-public-url` when the page is published remotely.

## Test

Run the standard formatting, lint, unit, frontend, and Helm checks through the
Makefile:

```sh
make format
make lint
make test
```

Kubernetes checks use the current kubeconfig and isolated test resources:

```sh
make test-kubernetes
make test-kubernetes-components
make test-kubernetes-ha
```

The Runtime, Agent, real-Gitea, and complete lifecycle targets require the
variables checked by each Make target, including a digest-pinned platform image
available to the cluster and reachable Gitea/network addresses:

```sh
make test-kubernetes-runtime
make test-kubernetes-agent
make test-kubernetes-gitea
make test-kubernetes-lifecycle
```

Run Dev Container interoperability against a native Docker daemon with:

```sh
make test-devcontainer-e2e-required
```

## License

[MIT](LICENSE).
