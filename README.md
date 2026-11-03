# Gitea Codespace

Gitea Codespace provides isolated, Dev Container-based development environments for Gitea repositories on Kubernetes. Users can create, stop, resume, and delete environments from Gitea, then work through the browser, SSH, or SFTP.

## Features

- Dev Container configuration from the selected repository and revision;
- Kata Containers or Sysbox isolation for Docker-based development workloads;
- browser-based development with code-server;
- SSH, SFTP, and HTTP endpoint access through a managed Gateway;
- reusable environment templates, per-site quotas, and optional registry caches;
- a web administration page for Gitea sites, environments, components, and active runtimes.

## Requirements

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- a Gitea installation with Codespace support;
- a working Kata Containers or Sysbox RuntimeClass labeled with its isolation type;
- a StorageClass compatible with the selected runtime.

Kata and Sysbox require node-level installation. Label each verified RuntimeClass so the administration page can associate it with the correct isolation mode:

```sh
kubectl label runtimeclass kata-qemu codespace.gitea.dev/isolation=kata
kubectl label runtimeclass sysbox-runc codespace.gitea.dev/isolation=sysbox
```

Verify the RuntimeClass, StorageClass, and Codespace image with a test Pod before enabling an environment template. The [Helm installation guide](charts/gitea-codespace/README.md) links to the runtime installation guides and documents the supported chart settings.

## Install

Create an administration token and install the development chart:

```sh
kubectl create namespace codespace-system
kubectl -n codespace-system create secret generic codespace-admin \
  --from-literal=token="$(openssl rand -hex 32)"

helm upgrade --install codespace \
  oci://ghcr.io/explodingdragon/charts/gitea-codespace \
  --devel \
  --namespace codespace-system \
  --set admin.tokenSecret.name=codespace-admin \
  --wait \
  --timeout 5m
```

The development chart uses `ghcr.io/explodingdragon/gitea-codespace:main`. For a reproducible installation, use the chart version and image digest published in a successful GitHub Actions workflow summary:

```sh
helm upgrade --install codespace \
  oci://ghcr.io/explodingdragon/charts/gitea-codespace \
  --version 0.1.0-main.REPLACE_WITH_RUN_NUMBER \
  --namespace codespace-system \
  --set image.digest=sha256:REPLACE_WITH_IMAGE_DIGEST \
  --set admin.tokenSecret.name=codespace-admin \
  --wait \
  --timeout 5m
```

## Configure

The administration Service is private by default. Open it through `kubectl`:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Visit <http://127.0.0.1:18080> and sign in with the administration token. Complete the initial configuration in this order:

1. Add a Gateway component and, when required, one or more Cache components.
2. Add environment templates using the verified runtime and storage classes discovered from the cluster.
3. Open the Codespace Managers page in Gitea and copy the registration token for the intended site-wide or personal scope.
4. Add a Gitea site with its display name, Gitea URL, and registration token. The Manager exchanges the token for a dedicated runtime identity and does not store the registration token.
5. Assign the site's environment templates, Gateway, optional caches, and resource quotas.
6. Enable the site after its status reports that the configuration is ready.

Users can then create Codespaces from supported repository pages in Gitea.

The administration page discovers RuntimeClasses, StorageClasses, Gateways,
and compatible listeners from the current cluster. Resource references are
selected from these results and revalidated when saved. CPU, memory, storage,
cache limits, and durations use numeric fields with explicit units so the
stored Kubernetes quantities remain valid without requiring administrators to
write quantity syntax directly.

Components are cluster-scoped resources managed through this page. The Manager
creates their Deployments, Services, credentials, network policies, and cache
storage in the management namespace. A site selects components by Kubernetes
identity, so deleting and recreating a component with the same name does not
silently reuse its former authorization.

## Operations

The Manager creates an installation identity in `Secret/codespace-gitea-codespace-internal-ca`. Back up this Secret and enable Kubernetes Secret encryption at rest. It must remain available across upgrades and Manager restarts.

Public administration access, Codespace endpoint routing, HTTPS, private image registries, identity recovery, and all Helm values are documented in the [Helm installation guide](charts/gitea-codespace/README.md).

## Development

Use the Go version declared in [`go.mod`](go.mod) and pnpm. The common development checks are:

```sh
make install-tools
make format
make lint
make test
make test-frontend
make build
```

See the [architecture and design documentation](https://github.com/ExplodingDragon/gitea-dev) before changing lifecycle, isolation, identity, or protocol behavior.

## License

[MIT](LICENSE).
