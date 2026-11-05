# Gitea Codespace

Gitea Codespace provides isolated, Dev Container-based development
environments for Gitea repositories on Kubernetes. Users create and manage
their environments from Gitea, then connect through a browser, SSH, or SFTP.

## What it provides

- Dev Container environments created from a repository and revision;
- Kata Containers or Sysbox workload isolation;
- browser, SSH, SFTP, and authorized forwarded-port access;
- reusable environment templates and an optional registry cache; and
- a web administration page for sites, templates, components, and runtimes.

## Requirements

- Kubernetes 1.31 or later;
- Helm 4.3 or later;
- a Gitea installation with Codespace support;
- a verified Kata Containers or Sysbox RuntimeClass; and
- a StorageClass compatible with the selected runtime.

Prepare and verify either Kata Containers or Sysbox before creating an
environment template. The [Helm guide](charts/gitea-codespace/README.md)
explains the required RuntimeClass labels and storage behavior.

## Quick start

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

Use `--version <chart-version>` for a reproducible release instead of the
development chart.

## Configure

The administration Service is private by default. Open it locally with:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Open <http://127.0.0.1:18080>, sign in with the administration token, and:

1. create a Gateway and any optional Cache components;
2. create an environment template from verified runtime and storage classes;
3. create a registration token on Gitea's Codespace Managers page;
4. register the Gitea site and assign its templates and components; and
5. enable the site after it reports that it is ready.

Users can then create Codespaces from supported repository pages in Gitea.

## Documentation

- [Helm installation and operations](charts/gitea-codespace/README.md)
- [Developer guide](DEVELOPMENT.md)
- [Architecture and design](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)

## License

[MIT](LICENSE)
