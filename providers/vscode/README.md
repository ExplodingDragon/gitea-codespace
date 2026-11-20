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
replaces the template version. Resume preserves user settings and comments,
adds missing provider defaults, and keeps telemetry disabled. Bundled extensions
are installed from the provider image and refreshed when their contents change,
so ordinary resumes do not reinstall them. The Gitea Codespace extension supplies
the platform's port and USB management integration.

```json
{
  "settings": {"editor.tabSize": 2},
  "extensions": ["golang.go", "redhat.vscode-yaml"]
}
```

## Editor defaults

The IDE session trusts its workspaces automatically because Codespace already
builds and executes the repository's Dev Container configuration. This enables
tasks, debugging, and extensions without another trust prompt; repository code
still needs to be trusted before creating the Codespace.

Built-in AI features, including GitHub Copilot, are disabled by default with
`chat.disableAIFeatures`. Set it to `false` in `settings` or in the editor to
enable them explicitly.

The provider includes Czech, German, Spanish, French, Italian, Japanese, Korean,
Polish, Brazilian Portuguese, Russian, Turkish, Simplified Chinese, and Traditional
Chinese language packs. They are installed locally before the IDE starts, so
startup does not need the extension marketplace for translations. The interface
uses the browser language unless a display language has been selected manually
with **Configure Display Language**. English is the fallback for unavailable
languages and untranslated messages. Language pack versions are pinned separately
from code-server because their release schedules differ.

Both extension installation and the IDE server run with telemetry disabled.
Preparation sets `telemetry.telemetryLevel` to `off`, including on resume.
Third-party extensions can have independent telemetry controls; consult their
documentation when installing them. These editor settings do not restrict network
access by workspace programs or extensions.

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
