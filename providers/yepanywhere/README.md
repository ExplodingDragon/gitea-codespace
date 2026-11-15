# Yep Anywhere provider

Image: `ghcr.io/explodingdragon/gitea-codespace-web-ide-yepanywhere:latest`

This provider runs [Yep Anywhere](https://github.com/kzahel/yepanywhere) inside
the Dev Container with the current repository registered as a project. Select
the image in an environment template and resolve it before saving.

The image contains the app, its dependencies and a private Node runtime for
glibc-based `linux/amd64` and `linux/arm64` containers. Claude Code retains the
upstream SDK and executable selection. Codex, OpenCode V1 and Pi are also bundled
so they work without a separate installation. For these three tools, an explicit
executable path takes priority, followed by upstream external discovery and then
the bundled program. This lets Dev Container Features supply a preferred version;
an invalid explicit path is reported instead of silently choosing another tool.
The private Node does not change the user's `PATH` or `HOME`.
Browser access uses the Codespace Gateway session, so opening the workspace
does not require a separate Yep Anywhere login.

## Dev Container configuration

Use `customizations.yepanywhere` to select backends and seed Yep Anywhere's own
settings. Supported backend IDs are `claude`, `claude-gateway`, `claude-ollama`,
`codex`, `codex-oss`, `opencode`, and `pi`. An omitted `providers` list enables
discovery of this set; Claude variants retain their upstream configuration
requirements. A supplied list selects a subset for both the UI and execution.
Models offered through OpenCode or Pi keep their own provider catalogs.

```jsonc
{
  "customizations": {
    "yepanywhere": {
      "providers": ["codex", "claude"],
      "settings": {
        "codexReasoningSummary": "detailed"
      }
    }
  }
}
```

Repository `providers` replaces the template's list. Repository settings
override template settings by top-level key. These values initialize the
provider once; subsequent starts retain changes made in the app. Yep Anywhere
data, including its SQLite database, is stored in the Codespace provider data
directory. Use storage supported by upstream SQLite; network filesystems such
as NFS are rejected during preparation rather than causing slow database access.

The app inherits the remote user's environment, declared secrets, proxy and CA
configuration. Agent-specific settings and credentials belong to the relevant
Feature or tool. Provider startup sets its own data directory, Gateway listener
and authentication mode. On first initialization, Codex automatic updates are
set to off; subsequent starts preserve the saved update preference.
Bundled programs update with the Provider image; Features and the tools' own
configuration manage external installations. The `settings` block accepts app
settings except `allowedHosts`, `codexUpdatePolicy`, `publicSharesEnabled`, and
`limitedUsersEnabled`. These fields are reserved for Provider initialization
and access configuration; supplying them makes preparation fail with an error.

## Workspace access

Gateway authenticates browser HTTP and WebSocket connections as the workspace
owner. This keeps authentication and session renewal aligned with the other
Codespace providers. The app's remote relay, server-to-server access, public
sharing and additional-user configuration are disabled, including when saved
data contains previously enabled settings. Their setup pages are hidden, and
access-configuration requests return an explicit error. This policy concerns
application access; agent tools, model APIs and MCP retain their own network
and authentication behavior.

## Ports and USB

Open **Codespace** in the sidebar to publish ports, edit labels and visibility,
or cancel publication. The **USB** tab appears when the Runtime provides USB/IP.
It groups devices by source and offers one connect or disconnect action per
device. The page refreshes automatically while visible and after each operation.

**Connection help** provides the current SSH command. When USB is available,
it also builds a reverse-forward command with a source name and keepalive
options. Run it on the USB/IP server machine and keep it connected while using
the devices. See the [USB/IP guide](../../docs/usbip.md) for server preparation.

Port labels accept up to 128 UTF-8 bytes. See the [provider overview](../README.md)
for image updates and digest references, and the
[development guide](../DEVELOPMENT.md#maintain-the-official-providers) for build
and validation instructions.
