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
Backends without an explicit connection keep their native configuration and
model catalogs.

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

The app inherits the remote user's environment, the secrets selected for this
instance, and proxy and CA configuration. Tools can use their native configuration
or the connection initialization below. Provider startup sets its own data
directory, Gateway listener and authentication mode. On first initialization,
Codex automatic updates are set to off; subsequent starts preserve the saved
update preference.
Bundled programs update with the Provider image; Features and the tools' own
configuration manage external installations. The `settings` block accepts app
settings except `allowedHosts`, `codexUpdatePolicy`, `publicSharesEnabled`, and
`limitedUsersEnabled`. These fields are reserved for Provider initialization
and access configuration; supplying them makes preparation fail with an error.

## Model API connections

Use `connections` to initialize the model services used by Yep Anywhere. Store
tokens in Gitea Codespace secrets and reference their environment variable names
with `apiKeyEnv`. This works with any injected name, including `CODEX_API_TOKEN`;
the Provider passes it to the selected tool using that tool's native interface.

```jsonc
{
  "secrets": {
    "COMPANY_API_TOKEN": {"description": "Company model service API token"}
  },
  "customizations": {
    "yepanywhere": {
      "providers": ["codex", "codex-oss", "claude", "opencode", "pi"],
      "connections": {
        "codex": {
          "baseUrl": "https://models.example.com/v1",
          "apiKeyEnv": "COMPANY_API_TOKEN",
          "models": [{"id": "company-coder", "name": "Company Coder"}]
        },
        "codex-oss": {
          "baseUrl": "http://model-server.example.com:8000/v1",
          "models": [{"id": "local-coder"}]
        },
        "claude": {
          "baseUrl": "https://models.example.com/anthropic",
          "apiKeyEnv": "COMPANY_API_TOKEN",
          "authMode": "bearer",
          "models": [{"id": "company-claude"}]
        },
        "opencode": {
          "baseUrl": "https://models.example.com/v1",
          "apiKeyEnv": "COMPANY_API_TOKEN",
          "api": "openai-responses",
          "models": [{"id": "company-coder"}]
        },
        "pi": {
          "baseUrl": "https://models.example.com/v1",
          "apiKeyEnv": "COMPANY_API_TOKEN",
          "api": "openai-completions",
          "models": [{"id": "company-chat", "contextWindow": 128000, "maxTokens": 16384}]
        }
      }
    }
  }
}
```

Each backend has its own connection; select only the backends and API protocols
your service supports. The examples use placeholder hosts and model IDs.

| Backend | API and authentication |
| --- | --- |
| `codex`, `codex-oss` | Responses API with Bearer authentication. Both use the configured URL. |
| `claude` | Anthropic Messages API. `authMode` defaults to `api-key` (`x-api-key`); `bearer` uses `Authorization: Bearer`. |
| `opencode`, `pi` | `api` is required: `openai-completions`, `openai-responses`, or `anthropic-messages`. |

`baseUrl` accepts an HTTP or HTTPS origin with an optional path prefix, without
embedded credentials, a query string or a fragment. For an OpenAI-compatible
service this is usually a URL ending in `/v1`; the Claude SDK normally adds
`/v1/messages` to its base. The Provider preserves the supplied path. Supply
credentials through `apiKeyEnv` so the saved configuration contains only their
variable name.

`models` is an optional list with unique `id` values and optional display `name`.
`defaultModel` selects an ID from that list; it defaults to the first entry.
When a connection supplies a catalog, sessions select models from that catalog,
keeping model selection on the intended service even when restoring old app data.
The catalog is available without contacting the model API; actual requests
report authentication, network or protocol errors when the tool runs.

Omitting `models` or supplying `[]` leaves the custom connection's catalog empty
and allows the IDE to start; it does not guarantee that the backend can start a
model session. Codex and Claude then use their native default model unless a
session explicitly selects one, and the configured API service must support
that model. For OpenCode and Pi, supply the service's available models in
`connections.<backend>.models` so sessions can select a model under `codespace`.
Custom API services may support different models, so their configured catalogs
define the choices shown in the IDE. To use a tool's native configuration and
model catalog instead, omit its connection or set it to `null`.

Optional `contextWindow` supplies catalog information. OpenCode and Pi also use
it for native model registration and accept `maxTokens` as the output limit;
their defaults are 128000 context tokens and up to 16384 output tokens, capped
by the context window. An explicit output limit must fit within the configured
or default context window. Set these values to the limits of the actual service.
Codex and Claude keep their native token-limit behavior. OpenCode and Pi display
these models under the `codespace` provider.

Omitting `apiKeyEnv` explicitly selects an unauthenticated endpoint. Tools that
require a non-empty API key receive the fixed, non-secret `codespace-no-auth`
placeholder. A named but missing or empty secret makes only that backend
unavailable; the IDE and other backends still start. Each tool launch reads the
current process environment. Codespace restores the secret values selected when
the instance was created, so changing a personal saved secret and restarting
the instance does not update those values. Platform access tokens are issued
separately for the runtime lifecycle. See the
[creation guide](../../README.md#create-a-codespace) for secret selection and retention.

Template connections provide defaults. Each repository backend entry replaces
the corresponding template entry in full, keeping its URL and credential choice
together. For example, `"connections": {"codex": null}` clears the template's
Codex connection and uses native Codex configuration. These settings are seeded
on the first initialization; later starts retain
`/var/lib/gitea-codespace/web-ide/codespace-options.json`. That file contains
parameters and variable names, not resolved tokens. To change a saved connection,
edit that file and restart the Codespace, or apply the changed Dev Container
configuration to a new Codespace.

Connection overrides apply only to tools started by Yep Anywhere. The tools'
native configuration, login files, session storage and external executable
selection remain intact, so CLI use continues with the user's own settings.

## Workspace access

Gateway authenticates browser HTTP and WebSocket connections as the workspace
owner. This keeps authentication and session renewal aligned with the other
Codespace providers. Workspace access stays on this authenticated Gateway path;
remote relay, server-to-server access, public sharing and additional-user setup
are unavailable. Agent tools, model APIs and MCP retain their own network and
authentication behavior.

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
