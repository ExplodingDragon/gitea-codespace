# VS Code Web IDE provider

This project packages code-server and the Gitea Codespace extension as the
official `vscode` provider. The extension is part of this provider because it
uses VS Code APIs, so the editor and extension are released together while the
Codespace services continue to use the stable Provider contract.

The provider accepts `settings` and `extensions` options. Environment-template
values are applied first and repository `customizations.vscode` values are
applied afterwards. Repository settings override matching template keys, while
an extension with the same identifier replaces the template version.

Run `make -C providers/vscode check` for the adapter and extension checks. Run
`make -C providers/vscode image` to download the pinned code-server release,
package the extension, and build a local image for the current architecture. The
dedicated GitHub workflow publishes linux/amd64 and linux/arm64 manifests under
`ghcr.io/explodingdragon/gitea-codespace-web-ide-vscode:main`.
