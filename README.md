# Gitea Codespace

Gitea Codespace provides the manager and gateway that create and operate remote
development environments for Gitea. A manager receives lifecycle operations
from Gitea, provisions Incus instances, prepares the repository's Dev Container,
and exposes authenticated web, SSH, SFTP, and forwarded-port access through its
gateway.

## Prerequisites

- A Gitea instance with Codespace support enabled.
- Go 1.26.4 or later to build from source.
- An Incus server reachable through a local Unix socket or a trusted HTTPS
  endpoint.
- An Incus storage pool and bridge network suitable for the selected container
  or virtual-machine environments.
- DNS for the gateway base domain and its wildcard subdomains and, for
  production deployments, a reverse proxy with TLS for the public gateway URL.

The manager can create both Incus containers and virtual machines. Virtual
machine images must include a working Incus agent because lifecycle commands,
file transfer, and runtime inspection use the Incus API.

## Build from source

```bash
go build -o gitea-codespace .
```

## Configuration

The manager uses YAML configuration. Start with the documented example:

```bash
cp examples/config.example.yaml codespace.yaml
```

The configuration has three top-level sections:

- `node` defines the manager identity, state directory, capacity, and worker
  behavior.
- `gateway` defines the public HTTP and SSH entry points.
- `runtime` defines Git and Web IDE behavior, image caching, the Incus
  connection, and the selectable runtime environments.

Each entry in `runtime.environments` declares a tag shown on the Codespace
creation page. It selects an Incus container or virtual-machine source and its
resource limits. See [`examples/config.example.yaml`](examples/config.example.yaml)
for all supported settings and deployment comments.

When `--config` is omitted, the command looks for `codespace.yaml` and then
`codespace.yml` in the current directory. Registration and serving must use the
same configuration and state directory.

## Register

Create a site-wide or personal manager registration token in the corresponding
Gitea Codespace Manager settings, then run:

```bash
./gitea-codespace register --config codespace.yaml
```

The command prompts for the Gitea URL and registration token. It exchanges the
registration credential for a manager identity and stores that identity in
`node.state_dir`. The registration token remains in Gitea and is not written to
the state directory. Protect and persist this directory; it also contains the
manager's local lifecycle state and gateway keys.

Use a Gitea URL that is reachable from the manager and from the development
instances. A loopback URL normally refers to the instance itself and therefore
cannot be used by a Codespace to clone its repository.

## Run

Start the registered manager and gateway with:

```bash
./gitea-codespace serve --config codespace.yaml
```

The process declares its configured environment tags, polls Gitea for lifecycle
operations, reconciles its Incus instances, and serves the configured gateway
listeners. Keep the process under a service supervisor and persist
`node.state_dir` across restarts.

The public gateway base domain, its `*.` wildcard, and the SSH address must
resolve to this deployment. A reverse proxy may terminate TLS for the HTTP
gateway, but it must preserve the original host and WebSocket connections used
by interactive endpoints.

## Architecture

- **Manager:** registers with Gitea, advertises environment tags, claims queued
  operations, and maintains local lifecycle state.
- **Provisioner:** creates and controls Incus instances and executes commands or
  file operations through the Incus API.
- **Dev Container runtime:** resolves the repository configuration, builds or
  pulls its images, starts the development container, injects approved secrets,
  and launches the Web IDE.
- **Gateway:** authenticates Gitea-issued access and proxies Web IDE, endpoint,
  SSH, SFTP, and local port-forwarding sessions without exposing an instance
  directly.

The public [`devcontainer`](devcontainer) package implements Dev Container
configuration and Docker execution independently of the manager so it can also
be used and tested as a Go API.

## Testing

Run tests that do not require a real Incus deployment:

```bash
make test
```

Run Incus end-to-end tests automatically when a usable local Incus client is
available:

```bash
make test-e2e-auto
```

The required targets fail instead of skipping when their infrastructure is not
available. They run serially and create one instance at a time:

```bash
make test-e2e-required
make test-e2e-manager-container-required
make test-e2e-manager-vm-required
make test-e2e-incus-matrix-required
```

Run the real Docker interoperability tests for the Dev Container implementation
with:

```bash
make test-devcontainer-e2e-required
```

## License

This project is licensed under the MIT License. See [`LICENSE`](LICENSE) for the
full text.
