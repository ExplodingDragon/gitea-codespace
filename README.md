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
- Supports persistent build caching and registry mirrors for repeated builds.
- Supplies official VS Code and DeepSeek Harness Web IDE providers while
  allowing compatible third-party providers.
- Manages multiple Gitea sites and their workspaces from one administration
  service.

## Requirements

- Kubernetes 1.31 or later
- Helm with OCI registry support
- A Gitea instance with Codespace support
- A StorageClass suitable for workspace data
- Kata or Sysbox installed when the corresponding isolation mode is selected

The workspace runtime and StorageClass must be tested together on the nodes
that will run Codespaces. A RuntimeClass can start a basic Pod successfully
while still lacking the nested-container or storage behavior required by a Dev
Container.

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
5. Enable the site after its readiness checks pass.

Gateway HTTP and SSH services remain internal until the cluster operator
publishes them. This keeps certificate, DNS, Ingress, and LoadBalancer policy
under the control of the Kubernetes deployment. The
[deployment guide](charts/gitea-codespace/README.md#publish-workspace-access)
describes the supported publishing model.

## Create a codespace

After the site is enabled:

1. Open a repository, commit, or pull request in Gitea.
2. Select **Create Codespace**.
3. Choose a Dev Container configuration and environment.
4. Open the workspace after creation completes.

The Codespace page lists the available Web IDE, SSH connection, and forwarded
application URLs. Stopping a Codespace releases its runtime resources while
retaining the workspace for its next start.

## Web IDE providers

Official providers are published independently from the platform image so an
editor can be updated without rebuilding Manager, Agent, Gateway, or Cache.
Environment templates resolve a selected provider image to an immutable digest
to keep existing environments reproducible.

| Provider         | Image                                                         |
| ---------------- | ------------------------------------------------------------- |
| VS Code          | `ghcr.io/explodingdragon/gitea-codespace-web-ide-vscode:main` |
| DeepSeek Harness | `ghcr.io/explodingdragon/gitea-codespace-web-ide-dsh:main`    |

See [Web IDE providers](providers/README.md) for configuration and third-party
provider integration.

## Versions

The `main` chart and image tags track the current development build and are
intended for evaluation before the first stable release. Published releases
use matching chart and application versions; install one with:

```sh
helm upgrade --install codespace \
  oci://ghcr.io/explodingdragon/charts/gitea-codespace \
  --version <version> \
  --namespace codespace-system \
  --set admin.tokenSecret.name=codespace-admin \
  --wait
```

Using the same release for the CRDs, chart, and platform image keeps the
Kubernetes API and controllers aligned. Follow the
[upgrade procedure](charts/gitea-codespace/README.md#upgrade) when moving an
existing installation to another version.

## Documentation

- [Install and operate with Helm](charts/gitea-codespace/README.md)
- [Configure or build a Web IDE provider](providers/README.md)
- [Develop and test Gitea Codespace](DEVELOPMENT.md)
- [Review the architecture and design](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)

## Contributing

Contributions are welcome through issues and pull requests. Start with the
[development guide](DEVELOPMENT.md), which documents the repository layout,
toolchain, generated sources, and the validation commands used by CI.

## License

Gitea Codespace is licensed under the [MIT License](LICENSE).
