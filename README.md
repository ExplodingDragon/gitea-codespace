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

The manager keeps its active site identity and runtime configuration in manager
state. A single-node deployment can use SQLite:

```bash
export GITEA_CODESPACE_STATE=local
export GITEA_CODESPACE_STATE_PATH=/var/lib/gitea-codespace/manager.db
export GITEA_CODESPACE_STATE_ENCRYPTION_KEY="$(openssl rand -base64 32)"
export GITEA_CODESPACE_NODE_ID=manager-01
export GITEA_CODESPACE_NODE_ROLE=all
export GITEA_CODESPACE_ADMIN_LISTEN=127.0.0.1:18080
export GITEA_CODESPACE_ADMIN_TOKEN="$(openssl rand -hex 32)"
```

Multi-node deployments can use etcd for the same state objects:

```bash
export GITEA_CODESPACE_STATE=etcd
export GITEA_CODESPACE_ETCD_ENDPOINTS=http://127.0.0.1:2379
export GITEA_CODESPACE_ETCD_PREFIX=/gitea-codespace
export GITEA_CODESPACE_STATE_ENCRYPTION_KEY="$(openssl rand -base64 32)"
export GITEA_CODESPACE_NODE_ID=gateway-01
export GITEA_CODESPACE_NODE_ROLE=gateway
export GITEA_CODESPACE_ADMIN_LISTEN=127.0.0.1:18080
export GITEA_CODESPACE_ADMIN_TOKEN="$(openssl rand -hex 32)"
```

`GITEA_CODESPACE_NODE_ROLE=all` starts both the worker and gateway in one
process. `GITEA_CODESPACE_NODE_ROLE=gateway` starts only the HTTP and SSH
gateway. This is useful when gateway nodes should be placed near the public
network edge while worker nodes keep Incus write access and lifecycle state
transitions in one place.

Run the local administration API and add the Gitea site identity created from
the Gitea Codespace Manager settings page:

```bash
./gitea-codespace admin
```

The active runtime configuration still has three top-level sections:

- `node` defines the manager name, state directory, capacity, and worker
  behavior.
- `gateway` defines the public HTTP and SSH entry points.
- `runtime` defines Git and Web IDE behavior, image caching, Incus backends,
  and the selectable runtime environments.

Each entry in `runtime.environments` declares a tag shown on the Codespace
creation page. It selects an Incus container or virtual-machine source and its
resource limits. See [`examples/config.example.yaml`](examples/config.example.yaml)
for all supported settings and deployment comments.

An existing YAML file can be imported once into an empty local state database by
starting `serve --config <path>`. After import, `serve` reads the state database;
the YAML file is only an input format for administrators and is not the live
runtime source.

## Manager identity

Create a site-wide or personal manager in the corresponding Gitea Codespace
Manager settings. Gitea shows the manager secret once when the identity is
created. Store that URL, manager ID, and secret through the local administration
API. The secret is encrypted before it is written to the state database.

The manager state database and `node.state_dir` both need to be protected and
persisted across restarts. The state database stores site identities and the
active runtime configuration. `node.state_dir` stores lifecycle state, the
latest inventory generation, and gateway keys.

Use a Gitea URL that is reachable from the manager and from the development
instances. A loopback URL normally refers to the instance itself and therefore
cannot be used by a Codespace to clone its repository.

## Run

Start the manager and gateway with:

```bash
./gitea-codespace serve
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

- **Manager:** authenticates to Gitea, advertises environment tags, claims
  queued operations, and maintains local lifecycle state.
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

Run the fast manager process smoke tests:

```bash
make test-smoke
```

Run manager state tests against a real etcd started by the Makefile:

```bash
make test-etcd-required
make test-etcd-cluster-required
```

`make test-etcd` runs both etcd targets when an `etcd` binary is available and
otherwise prints a skip reason. The required targets fail if etcd is missing,
which makes them suitable for CI or deployment validation.

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
