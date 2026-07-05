# codespace

Codespace manager and gateway client for Gitea.

## What It Contains

- Gateway user API for open flow, codespace view, and preview ports.
- Runtime API for codespace init status and port declarations.
- Manager worker that declares itself to Gitea, polls tasks, and executes lifecycle actions.
- Dummy provisioner used to simulate Incus-backed codespace during development and tests.

## Current Behavior

- `register` consumes a Gitea manager registration token once and writes the returned manager credentials to `codespace.yaml`.
- `serve` reads `codespace.yaml`, starts the local gateway/runtime API, and connects to Gitea with the saved manager UUID and token.
- Runtime state is kept in memory; Gitea remains the control-plane source of truth.

## Register

```bash
go run ./cmd/gitea-codespace register
```

The command prompts for the Gitea URL, registration token, and manager name. It writes `codespace.yaml` in the current directory.

## Serve

```bash
go run ./cmd/gitea-codespace serve
```

Default gateway listen address:

```text
http://127.0.0.1:18080
```

Useful endpoints:

- `GET /api/codespace/{id}/open`
- `GET /api/runtime/context`
- `POST /api/runtime/ports`
