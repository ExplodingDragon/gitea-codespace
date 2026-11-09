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

Select an image in an environment template and resolve it before saving. The
resolved provider name, version, supported platforms, and configuration fields
come from the image itself. A compatible third-party provider can be used in
the same way.

Provider data is stored with the codespace and reused across normal starts.
Changing the image or its configuration affects new runtime preparation; it
does not couple the editor release to Manager, Agent, or Gateway.

## Develop a provider

The [provider development guide](DEVELOPMENT.md) defines the image metadata,
runtime bundle, adapter protocol, build process, and verification steps. The
official [VS Code](vscode/README.md) and [DeepSeek Harness](dsh/README.md)
providers are production examples. The deterministic provider under
[`tests/e2e/web-ide`](../tests/e2e/web-ide) is the smallest complete reference
for protocol and Kubernetes lifecycle tests.
