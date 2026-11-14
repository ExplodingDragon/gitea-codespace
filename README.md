# Gitea Codespace

[![CI](https://github.com/ExplodingDragon/gitea-codespace/actions/workflows/ci.yml/badge.svg)](https://github.com/ExplodingDragon/gitea-codespace/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/ExplodingDragon/gitea-codespace)](go.mod)
[![License](https://img.shields.io/github/license/ExplodingDragon/gitea-codespace)](LICENSE)

Gitea Codespace provides Kubernetes-hosted development environments for Gitea
repositories. Each environment is created from the repository's Dev Container
configuration and can be used from a browser, a desktop editor, SSH, or SFTP.

## Features

- Builds environments from Dev Container images, Dockerfiles, Docker Compose,
  and Features.
- Preserves workspace data across normal stops and starts.
- Provides Web IDE, SSH, SFTP, and forwarded application access through a
  shared Gateway.
- Supports Kata Containers, Sysbox, and the cluster's native container runtime.
- Connects remote USB devices to compatible Kata environments through the
  existing authenticated SSH endpoint.
- Supports persistent build caching and registry mirrors for repeated builds.
- Supplies official VS Code and DeepSeek Harness Web IDE providers while
  allowing compatible third-party providers.
- Manages multiple Gitea sites and their workspaces from one administration
  service.

## Requirements

- Kubernetes 1.36 or later with image-volume subdirectory support in the CRI
  runtime
- Helm with OCI registry support
- A Gitea instance with Codespace support
- A StorageClass suitable for workspace data
- Kata or Sysbox installed when the corresponding isolation mode is selected

See the [Helm deployment guide](charts/gitea-codespace/README.md) for runtime
preparation, production networking, storage, backup, and upgrade procedures.

## Quick start

Create the namespace and an administration token. Keeping the token in a
Secret prevents it from being recorded in Helm values and release history.

```sh
kubectl create namespace codespace-system
kubectl -n codespace-system create secret generic codespace-admin \
  --from-literal=token="$(openssl rand -hex 32)"
```

Install the current `main` prerelease from the OCI chart registry:

```sh
helm upgrade --install codespace \
  oci://ghcr.io/explodingdragon/charts/gitea-codespace \
  --devel \
  --namespace codespace-system \
  --set admin.tokenSecret.name=codespace-admin \
  --wait \
  --timeout 5m
```

The administration service is private to the cluster by default. Port-forward
it for the initial configuration:

```sh
kubectl -n codespace-system port-forward \
  service/codespace-gitea-codespace-admin 18080:18080
```

Open <http://127.0.0.1:18080> and sign in with the token stored in
`Secret/codespace-admin`.

## Connect Gitea

Complete the initial configuration in the administration interface:

1. Create a Gateway and, when needed, a build Cache.
2. Create an environment template using a verified runtime and StorageClass.
3. Create a registration token in Gitea's Codespace Managers page.
4. Register the Gitea URL and token, then assign the environment and
   components.
5. Configure the site's [network access](charts/gitea-codespace/README.md#configure-network-access)
   for repository cloning and build dependencies. Internet access is disabled
   by default.
6. Enable the site, then confirm that the site and its assigned resources are
   ready before creating a Codespace.

Gateway HTTP and SSH services remain internal until the cluster operator
publishes them. See the [deployment
guide](charts/gitea-codespace/README.md#publish-workspace-access) for Ingress,
DNS, TLS, and SSH exposure.

## Create a codespace

After the site is enabled:

1. Open a repository, commit, or pull request in Gitea.
2. Select **Create Codespace**.
3. Choose a Dev Container configuration and environment.
4. Open the workspace after creation completes.

The Codespace page lists the available Web IDE, SSH connection, and forwarded
application URLs. Stopping a Codespace releases its runtime resources while
retaining the workspace for its next start.

## Publish application ports

VS Code and DeepSeek Harness include a Ports interface. The same operations are
available from any Dev Container through `gitea-codespace-endpoint`, which is
useful for scripts and other editors:

```sh
gitea-codespace-endpoint list
gitea-codespace-endpoint set 3000 --label "Web application"
gitea-codespace-endpoint set 8080 --label "Public preview" --public
gitea-codespace-endpoint delete 8080
```

Authenticated ports require the current Gitea session. Use `--public` only for
services that are intended to be reachable without that session. The Codespace
page shows the resulting Gateway URL for every published port.

## Connect a remote USB device

Compatible Kata environments can connect remote USB devices through the
Codespace SSH endpoint. Manage devices from the bundled IDE plugins or the
Dev Container command line. See [Connect USB devices](docs/usbip.md) for setup,
multiple device sources, and reconnecting after a stop.

## Web IDE providers

Choose a Web IDE provider in the environment template when browser access is
needed. See [Web IDE providers](providers/README.md) for the official images,
editor configuration, and third-party provider integration.

## Versions

The platform image's `main` tag tracks the current development build. Pin a
published chart version to select its platform image digest. Web IDE providers
are versioned independently; choose a provider digest to also fix the editor
content, as described in the [provider guide](providers/README.md).
See the [Helm guide](charts/gitea-codespace/README.md#install)
for version selection and the [upgrade procedure](charts/gitea-codespace/README.md#upgrade)
for an existing installation.

## Documentation

- [Install and operate with Helm](charts/gitea-codespace/README.md)
- [Choose and configure a Web IDE provider](providers/README.md)
- [Connect over SSH and SFTP](docs/ssh.md)
- [Connect USB devices](docs/usbip.md)
- [Develop and test Gitea Codespace](DEVELOPMENT.md)
- [Review the architecture and design](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)

## Contributing

Contributions are welcome through issues and pull requests. Start with the
[development guide](DEVELOPMENT.md), which documents the repository layout,
toolchain, generated sources, and the validation commands used by CI.

## License

Gitea Codespace is licensed under the [MIT License](LICENSE).
