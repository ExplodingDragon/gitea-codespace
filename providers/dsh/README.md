# DeepSeek Harness Web IDE provider

This image provides DeepSeek Harness through the `dsh` provider. It runs inside
the Dev Container, keeps its profile with the codespace, and relies on
Codespace Gateway for browser authentication. The upstream build and its
Codespace startup integration are released with this image rather than the
Codespace platform.

## Workspace startup

DSH opens the current repository as a workspace and uses its root as the process
working directory. The registration persists across provider and codespace
restarts while other workspaces created in DSH remain unchanged.

## Configuration

The provider accepts `plugins` and `settings` options. Settings are keyed by an
existing top-level DSH profile entry ID and contain that entry's complete
configuration. Repository `customizations.dsh.settings` replaces the template
configuration for a matching entry ID, following DSH profile semantics. The
provider installs plugin packages with the official DSH package command.

```json
{
  "plugins": ["@example/dsh-plugin"],
  "settings": {
    "ui-theme": {"preference": "dark", "fontSize": 14}
  }
}
```

The provider validates the initial profile and then preserves changes made
through DSH. A setting for an unavailable plugin entry produces a warning and
is skipped; an invalid setting for an active entry reports an initialization
error. Repository settings replace template settings for the same profile entry.

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

## Development

Run `make -C providers/dsh check` for focused validation. See the [provider
development guide](../DEVELOPMENT.md) for build, image, and publishing details.
