# VS Code Web IDE provider

This image provides code-server and the Gitea Codespace extension through the
`vscode` provider. The extension is released with the editor because it uses VS
Code APIs, while the Codespace platform only depends on the provider protocol.

Use a Dev Container that meets the [official provider requirements](../README.md#official-providers).

## Configuration

The provider accepts `settings` and `extensions` options for initial setup.
Environment-template values are applied first and repository
`customizations.vscode` values are applied afterwards. Repository settings
override matching template keys, while an extension with the same identifier
replaces the template version. Resume preserves the persisted configuration
so user changes survive stops and Pod replacements. The bundled Gitea Codespace
extension is maintained by the provider because it supplies the platform's
port and USB management integration.

```json
{
  "settings": {"editor.tabSize": 2},
  "extensions": ["golang.go", "redhat.vscode-yaml"]
}
```

## SSH access

Use **Copy SSH command** in the Ports view or command palette to get the current
workspace's SSH command. Run it on your local computer. This action is available
in all runtime modes; the [SSH guide](../../docs/ssh.md) also covers SFTP, desktop
editors, port forwarding, and connection environment variables.

## USB devices

On compatible Kata runtimes, the bundled extension adds **USB Devices** to
Explorer and USB actions to the command palette. Use **Copy USB tunnel command**
in that view or the command palette to choose a source name and establish the
workstation's [SSH reverse tunnel](../../docs/usbip.md#connect-a-source), then use
**Attach USB device** to choose a remote device. The view shows imported devices
grouped by source and supports refresh and detach. The device picker includes
the source name to distinguish identical bus IDs on different servers and
excludes devices already imported from that source. USB actions appear when
the runtime provides the USB command.

Detach requires confirmation; unmount disks first. The
[USB guide](../../docs/usbip.md#attach-and-detach-devices) explains device status,
refresh failures, and reconnecting a source.

## Development

Run `make -C providers/vscode check` for focused validation. See the [provider
development guide](../DEVELOPMENT.md) for build, image, and publishing details.
