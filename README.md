# Gitea Codespace

Gitea Codespace provides isolated, Dev Container-based development
environments for Gitea repositories on Kubernetes. Users create and manage
their environments from Gitea, then connect through a browser, SSH, or SFTP.

## Capabilities

- load a Dev Container configuration from the selected repository and revision;
- run development workloads with Kata Containers or Sysbox isolation;
- provide browser development through code-server;
- expose authorized HTTP, SSH, and SFTP access through a managed Gateway;
- reuse administrator-defined environment templates and registry caches; and
- manage Gitea sites, environments, components, and active runtimes from a web
  administration page.

## Requirements

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- a Gitea installation with Codespace support;
- a verified Kata Containers or Sysbox RuntimeClass; and
- a StorageClass compatible with the selected runtime.

Install either Kata Containers or Sysbox before installing Codespace. The
selected runtime must support the cluster's Kubernetes version, operating
system, architecture, and container runtime. Follow the
[workload runtime guide](charts/gitea-codespace/README.md#install-a-workload-runtime)
to install and verify the runtime and storage combination.

## Install

Create an administration token and install the newest development chart:

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

Add `--version <chart-version>` to select an exact published chart and its
digest-pinned platform image.

## Configure

The administration Service is private by default. Open it locally with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Visit <http://127.0.0.1:18080>, sign in with the administration token, and
complete the initial configuration:

1. Add a Gateway component and any required Cache components.
2. Add environment templates using verified runtime and storage classes.
3. Create a registration token from the Codespace Managers page in Gitea.
4. Register the Gitea site with its URL and token.
5. Assign templates, the Gateway, optional caches, and optional site quotas.
6. Enable the site after its status reports that it is ready.

Users can then create Codespaces from supported repository pages in Gitea.

## Documentation

- [Helm installation and operations](charts/gitea-codespace/README.md)
- [Developer guide](DEVELOPMENT.md)
- [Architecture and design](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)

## License

[MIT](LICENSE)
