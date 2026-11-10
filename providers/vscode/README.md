# VS Code Web IDE provider

This image provides code-server and the Gitea Codespace extension through the
`vscode` provider. The extension is released with the editor because it uses VS
Code APIs, while the Codespace platform only depends on the provider protocol.

## Configuration

The provider accepts `settings` and `extensions` options. Environment-template
values are applied first and repository `customizations.vscode` values are
applied afterwards. Repository settings override matching template keys, while
an extension with the same identifier replaces the template version.

```json
{
  "settings": {"editor.tabSize": 2},
  "extensions": ["golang.go", "redhat.vscode-yaml"]
}
```

The Gitea Codespace extension is always installed because it provides the port
management integration expected by the platform.

### Implementation acceptance criteria

- Template settings are applied when repository customizations omit the same
  key, while repository settings take precedence for matching keys.
- Extensions from both sources are installed once per identifier, using the
  repository version when both sources select the same extension.
- The Codespace extension is present after provider preparation.

## Development

Run `make -C providers/vscode check` for the adapter and extension checks. Run
`make -C providers/vscode image` to download the pinned code-server release,
package the extension, and build a local image for the current architecture. The
dedicated GitHub workflow publishes linux/amd64 and linux/arm64 manifests under
`ghcr.io/explodingdragon/gitea-codespace-web-ide-vscode:main`.
