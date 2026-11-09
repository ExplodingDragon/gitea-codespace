# DeepSeek Harness Web IDE provider

This image provides DeepSeek Harness through the `dsh` provider. It runs inside
the Dev Container, keeps its profile with the codespace, and relies on
Codespace Gateway for browser authentication. The upstream build and its
Codespace startup integration are released with this image rather than the
Codespace platform.

## Workspace startup

The provider enters the current repository and registers its absolute path in
the native DSH Workspace Registry before publishing the browser surface.
Entering the directory gives project-relative tools the correct process
context, while registry registration makes the same project visible in the DSH
workspace list. Registration is idempotent, so restarting the provider
preserves one workspace record and leaves any other user-created workspaces
unchanged.

### Implementation acceptance criteria

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
adapter translates this structured input into the native profile layer so
templates do not depend on Cordis composition syntax. Plugin packages continue
to use the official DSH package command. Web startup and connection entries
remain provider-owned so template settings cannot replace the loopback and
trusted-proxy configuration required by Gateway.

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

## Development

Run `make -C providers/dsh check` for adapter and script checks. Run
`make -C providers/dsh image` on a host matching the requested architecture to
build the pinned upstream release and a local provider image. The dedicated
GitHub workflow uses native amd64 and arm64 runners and publishes the combined
image as `ghcr.io/explodingdragon/gitea-codespace-web-ide-dsh:main`.
