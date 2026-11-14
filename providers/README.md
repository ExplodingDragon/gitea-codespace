# Web IDE providers

Web IDEs are distributed as separate OCI images so an administrator can update
an editor without rebuilding the Codespace platform. For an enabled Web IDE,
Manager validates image metadata when creating a template or changing its
Web IDE configuration, and retains the selected tag or explicit digest.
Other template edits keep the validated Web IDE configuration so they do not
depend on registry availability.
An image without a tag uses `latest`; short names follow
[Docker's reference rules](https://docs.docker.com/reference/cli/docker/image/tag/)
and expand to Docker Hub, for example `team/ide` becomes `docker.io/team/ide:latest`.

## Official providers

| Provider | Image | Configuration |
| --- | --- | --- |
| VS Code | `ghcr.io/explodingdragon/gitea-codespace-web-ide-vscode:latest` | [Settings and extensions](vscode/README.md) |
| DeepSeek Harness | `ghcr.io/explodingdragon/gitea-codespace-web-ide-dsh:latest` | [Settings and plugins](dsh/README.md) |

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
Existing codespaces keep the image reference and options copied into their
runtime snapshot. Each new or replacement Pod, including after stop and resume,
pulls tags with `Always`; a running Pod keeps its mounted image. Explicit
`@sha256:...` references use `IfNotPresent` and select fixed content.

Official releases publish `latest` and the bundle version (for example,
`0.2.2-rc.3` for DSH) to the same multi-platform image. A version tag can receive
adapter or plugin fixes for that editor version, so use a digest when exact
reproducibility is required. Nodes reuse cached layers when the content matches.

## Develop a provider

The [provider development guide](DEVELOPMENT.md) defines the image metadata,
runtime bundle, adapter protocol, build process, and verification steps. The
official [VS Code](vscode/README.md) and [DeepSeek Harness](dsh/README.md)
providers are production examples. The deterministic provider under
[`tests/e2e/web-ide`](../tests/e2e/web-ide) is the smallest complete reference
for protocol and Kubernetes lifecycle tests.
