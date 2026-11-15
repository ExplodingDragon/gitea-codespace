# Yep Anywhere provider

Image: `ghcr.io/explodingdragon/gitea-codespace-web-ide-yepanywhere:latest`

This provider runs [Yep Anywhere](https://github.com/kzahel/yepanywhere) inside
the Dev Container with the current repository registered as a project. Select
the image in an environment template and resolve it before saving.

The image contains the app, its dependencies and a private Node runtime for
glibc-based `linux/amd64` and `linux/arm64` containers. Upstream dependencies
may include their own binaries. Install additional agent tools such as Codex
through Dev Container Features, and use those Features to configure models,
credentials and tool versions. Yep Anywhere keeps its normal tool discovery
and login behavior; its private Node does not change the user's `PATH` or `HOME`.

## Dev Container configuration

Use `customizations.yepanywhere` to select backends and seed Yep Anywhere's own
settings. Backend IDs follow the selected Yep Anywhere release; an omitted
`providers` list preserves the upstream discovery behavior.

```jsonc
{
  "customizations": {
    "yepanywhere": {
      "providers": ["codex", "claude"],
      "settings": {
        "publicSharesEnabled": false
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
and authentication mode, and initializes Codex automatic updates to off so tool
versions remain under user control. `allowedHosts` and `codexUpdatePolicy` are
reserved initialization settings. The app's optional relay and sharing features
retain their own authentication and prerequisites.

## Ports and USB

Open **Codespace** in the sidebar to publish ports, edit labels and visibility,
or cancel publication. The **USB** tab appears when the Runtime provides USB/IP.
It groups devices by source and offers one connect or disconnect action per
device. The active, visible page refreshes after each operation and every ten
seconds after the preceding read finishes.

**Connection help** provides the current SSH command. When USB is available,
it also builds a reverse-forward command with a source name and keepalive
options. Run it on the USB/IP server machine and keep it connected while using
the devices. See the [USB/IP guide](../../docs/usbip.md) for server preparation.

These controls invoke the existing Codespace CLI as the remote user. They are
available to the full Yep Anywhere user; limited users and public shares do not
receive Codespace management access.

## Build and validation

Use the Node version in `.node-version` and pnpm version in `version.env`:

```sh
make -C providers/yepanywhere check
make -C providers/yepanywhere image IMAGE=registry.example.com/yepanywhere:test
```

Build on the target architecture because upstream dependencies include native
code. The independent workflow publishes version and `latest` tags for both
architectures. The [provider guide](../README.md) explains update behavior and
digest references; the [development guide](../DEVELOPMENT.md) covers mounted
bundle validation and the adapter lifecycle.
