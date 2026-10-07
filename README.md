# Gitea Codespace

Gitea Codespace runs isolated, Dev Container-based development environments on
Kubernetes. This repository contains the Manager, Runtime Agent, Gateway,
build Cache, administration UI, and the reusable [`devcontainer`](devcontainer)
Go package.

Gitea owns users, repository permissions, and lifecycle intent. The Manager
owns Kubernetes resources and runtime execution. Each Runtime Pod uses a
dedicated Docker daemon under Kata Containers or Sysbox isolation; it receives
neither a Kubernetes API token nor a host container-runtime socket.

See the [design repository](https://github.com/ExplodingDragon/gitea-dev) for
architecture, security boundaries, and deployment requirements.

## Build

Requirements:

- Go 1.26.4 or later;
- pnpm;
- a Kubernetes cluster for deployment and integration tests;
- `kubectl` access that can install the Codespace CRDs.

```sh
make build
make generate-kubernetes
```

`make build` compiles and embeds the Vue administration UI. Build the Manager
and Runtime images with:

```sh
make images
```

## Deploy

The supplied deployment resources are:

- [`deploy/crds`](deploy/crds) for the custom resource definitions;
- [`deploy/identity.yaml`](deploy/identity.yaml) for an example internal
  certificate issuer;
- [`deploy/manager.yaml`](deploy/manager.yaml) for Manager RBAC, Deployment,
  Services, network policy, and leader routing;
- [`deploy/Dockerfile.manager`](deploy/Dockerfile.manager) and
  [`deploy/Dockerfile.runtime`](deploy/Dockerfile.runtime) for release images.

Before applying them, configure digest-pinned images, the administration URL,
the administrator token Secret, and the identity issuer. The cluster also needs
at least one verified RuntimeClass and StorageClass pair.

```sh
kubectl apply -f deploy/crds
kubectl apply -f deploy/identity.yaml
kubectl apply -f deploy/manager.yaml
```

For local development against an existing cluster:

```sh
./bin/gitea-codespace serve \
  --kubeconfig "$HOME/.kube/config" \
  --admin-token-file /path/to/admin-token \
  --certificate-directory /path/to/projected/identity
```

The identity directory contains `tls.crt`, `tls.key`, and `ca.crt`. The local
administration page defaults to <http://127.0.0.1:18080>. A remote page requires
HTTPS and an explicit `--admin-public-url`.

Use the administration UI to create Gitea sites, environment templates,
Gateways, and Caches. Secret values are write-only. Template changes apply to
new environments; existing Codespaces retain the inputs needed to resume their
persistent data.

## Test

Run formatting, static checks, unit tests, and frontend checks through the
project Makefile:

```sh
make format
make lint
make test
```

Kubernetes integration targets use the current kubeconfig and create isolated
test resources:

```sh
make test-kubernetes
make test-kubernetes-components
make test-kubernetes-ha
```

Runtime and product lifecycle tests additionally require a digest-pinned
Runtime image imported into the cluster, a verified RuntimeClass/StorageClass
pair, and network addresses reachable from Runtime Pods. The Makefile validates
their required variables before running:

```sh
make test-kubernetes-runtime
make test-kubernetes-agent
make test-kubernetes-gitea
make test-kubernetes-lifecycle
```

Run the independent Dev Container interoperability suite with a native Docker
daemon:

```sh
make test-devcontainer-e2e-required
```

## License

[MIT](LICENSE).
