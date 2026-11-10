# VS Code Web IDE provider

This image provides code-server and the Gitea Codespace extension through the
`vscode` provider. The extension is released with the editor because it uses VS
Code APIs, while the Codespace platform only depends on the provider protocol.

## Configuration

The provider accepts `settings` and `extensions` options for initial setup.
Environment-template values are applied first and repository
`customizations.vscode` values are applied afterwards. Repository settings
override matching template keys, while an extension with the same identifier
replaces the template version. Resume preserves the persisted configuration
so user changes survive stops and Pod replacements.

```json
{
  "settings": {"editor.tabSize": 2},
  "extensions": ["golang.go", "redhat.vscode-yaml"]
}
```

The initial preparation merges template and repository values, with repository
values taking precedence. Later starts preserve editor settings, extensions,
and other user state. The bundled Gitea Codespace extension is kept current
because it provides the platform's port management integration.

## Development

Run `make -C providers/vscode check` for focused validation. See the [provider
development guide](../DEVELOPMENT.md) for build, image, and publishing details.
