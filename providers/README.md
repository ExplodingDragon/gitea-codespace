# Web IDE providers

Web IDEs are distributed as separate OCI images so an administrator can update
an editor without rebuilding the Codespace platform. Manager resolves the
selected image to an immutable digest when an environment template is saved,
which keeps existing templates on the reviewed provider release.

## Official providers

| Provider | Image | Configuration |
| --- | --- | --- |
| VS Code | `ghcr.io/explodingdragon/gitea-codespace-web-ide-vscode:main` | [Settings and extensions](vscode/README.md) |
| DeepSeek Harness | `ghcr.io/explodingdragon/gitea-codespace-web-ide-dsh:main` | [Settings and plugins](dsh/README.md) |

Both official providers support `linux/amd64` and `linux/arm64` Dev Containers
using glibc. The editor runs inside the Dev Container, so that image must supply
compatible system libraries; the carrier image supplies read-only program files.
Third-party providers define their own program compatibility requirements.

Select an image in an environment template and resolve it before saving. The
resolved provider name, version, supported platforms, and configuration fields
come from the image itself. A compatible third-party provider can be used in
the same way.

Provider data is stored with the codespace and reused across normal starts.
Changing an environment template affects codespaces created after that save.
Existing codespaces keep the provider digest and options copied into their
runtime snapshot, including after a stop, resume, or Pod replacement. This
keeps an editor release independent from Manager, Agent, and Gateway while
making each existing environment reproducible.

## Develop a provider

The [provider development guide](DEVELOPMENT.md) defines the image metadata,
runtime bundle, adapter protocol, build process, and verification steps. The
official [VS Code](vscode/README.md) and [DeepSeek Harness](dsh/README.md)
providers are production examples. The deterministic provider under
[`tests/e2e/web-ide`](../tests/e2e/web-ide) is the smallest complete reference
for protocol and Kubernetes lifecycle tests.
