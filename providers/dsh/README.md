# DeepSeek Harness Web IDE provider

This image provides DeepSeek Harness through the `dsh` provider. It runs inside
the Dev Container, keeps its profile with the codespace, and relies on
Codespace Gateway for browser authentication. The upstream build and its
Codespace startup integration are released with this image rather than the
Codespace platform.

## Workspace startup

The provider enters the current repository and registers its absolute path in
the native DSH Workspace Registry before publishing the browser endpoint.
Entering the directory gives project-relative tools the correct process
context, while registry registration makes the same project visible in the DSH
workspace list. Registration is idempotent, so restarting the provider
preserves one workspace record and leaves any other user-created workspaces
unchanged.

### Expected behavior

- Opening DSH for the first time shows the current repository as a workspace.
- Restarting DSH retains one registration for the repository and starts from
  the repository root.
- A missing repository directory stops provider startup with a direct error
  while the rest of the codespace remains available.

## Configuration

The provider accepts `plugins` and `settings` options. Settings are keyed by an
existing top-level DSH profile entry ID and contain that entry's complete
configuration. Repository `customizations.dsh.settings` replaces the template
configuration for a matching entry ID, following DSH profile semantics. The
adapter translates this structured input into DSH's native profile format.
Plugin packages continue to use the official DSH package command. The provider
keeps its connection settings separate from template options so it continues
to use the loopback address and trusted proxy mode required by Gateway.

```json
{
  "plugins": ["@example/dsh-plugin"],
  "settings": {
    "ui-theme": {"preference": "dark", "fontSize": 14}
  }
}
```

The generated profile is validated before it becomes persistent. Later starts
reuse that profile, which preserves changes made through the DSH settings and
plugin interfaces instead of reapplying template defaults. A setting whose
top-level entry is unavailable after plugin installation is skipped with a
warning. This lets one template work across provider or plugin versions while
keeping unsupported configuration out of the persistent profile. A setting
that matches an entry but is invalid still stops Web IDE initialization so an
incorrect active configuration is not published.

### Expected behavior

- Plugins from the template and repository are installed through the DSH
  package command.
- Repository settings replace template settings for the same profile entry.
- Supported settings persist in the web profile, missing entries produce a
  warning, and invalid active entries stop initialization with a direct error.

### Use an OpenAI-compatible service

A repository can supply the address, API protocol, and model catalog for an
OpenAI-compatible service while asking Gitea to provide the API token as a
Codespace secret. This keeps the shared Dev Container configuration useful to
every contributor without storing a token in Git.

Add the following structure to `.devcontainer/devcontainer.json`, replacing
the example address and model ID with values accepted by the service:

```jsonc
{
  "secrets": {
    "COMPANY_OPENAI_TOKEN": {
      "description": "API token for the company OpenAI-compatible service"
    }
  },
  "customizations": {
    "dsh": {
      "settings": {
        "llm-pi-ai": {
          "providers": {
            "company-openai": {
              "apiKeyEnv": "COMPANY_OPENAI_TOKEN",
              "api": "openai-completions",
              "baseURL": "https://llm.example.com/v1",
              "models": [{"id": "company-model"}]
            }
          }
        }
      }
    }
  }
}
```

`apiKeyEnv` names the environment variable that DSH reads; its value comes
from the Codespace secret rather than this file. Use `openai-completions` when
the service implements OpenAI Chat Completions, or `openai-responses` when it
implements the OpenAI Responses API. The `baseURL` and every model `id` must
match the service's API documentation.

Before creating the codespace:

1. Open **User settings → Codespaces → Secrets** in Gitea.
2. Create `COMPANY_OPENAI_TOKEN` and grant it access to the repository. The
   create page also offers a declared secret that has not been configured yet.
3. Create the codespace and confirm the secret selection.
4. Open DSH and select `company-openai` and the configured model.

Each key under `settings` is a complete DSH profile entry. If the repository
already configures `llm-pi-ai`, place every required provider in the same
`providers` object because the repository entry replaces an environment
template entry with the same ID. The repository configuration initializes a
new codespace once; later starts retain changes made through DSH. Create a new
codespace to apply changed repository defaults, or use **Settings → Models** in
DSH to adjust an existing codespace.

### Expected behavior

- Gitea offers the declared token during codespace creation and supplies it to
  DSH as `COMPANY_OPENAI_TOKEN`.
- DSH lists the configured provider and models without persisting the token in
  the repository configuration.
- Restarting the codespace retains the DSH profile and uses the currently
  authorized secret value.

## Codespace ports

The bundled Codespace plugin provides a **Ports** page for listing, publishing,
editing, and removing Gateway endpoints. It uses the same runtime endpoint API
as the Dev Container command, so changes made in DSH, VS Code, the command line,
or an application script all update one authoritative list.

The `/ports` command exposes the same operations in a DSH conversation:

```text
/ports
/ports publish 3000 authenticated Web application
/ports publish 8080 public Public preview
/ports remove 8080
```

The model can use `codespace_list_ports`, `codespace_publish_port`, and
`codespace_unpublish_port` when a task needs to inspect or change published
ports. Authenticated access is the default; public access must be selected
explicitly because it allows requests without a Codespace user session.

### Expected behavior

- The Ports page, `/ports` command, and model tools show the same endpoint set.
- Publishing or editing a port updates its label and visibility without
  restarting the application that listens on it.
- Removing a port removes Gateway access without stopping the application.

## Development

Run `make -C providers/dsh check` for adapter, plugin, and script checks. Run
`make -C providers/dsh image` on a host matching the requested architecture to
build the pinned upstream release and a local provider image. The dedicated
GitHub workflow uses native amd64 and arm64 runners and publishes the combined
image as `ghcr.io/explodingdragon/gitea-codespace-web-ide-dsh:main`.
