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

The manager keeps all enabled Gitea site identities and its runtime
configuration in manager state. A single-node deployment uses embedded etcd by default:

```bash
export GITEA_CODESPACE_STATE=embedded
export GITEA_CODESPACE_STATE_PATH=/var/lib/gitea-codespace/etcd
export GITEA_CODESPACE_STATE_ENCRYPTION_KEY='<same key as the execution node>'
export GITEA_CODESPACE_NODE_ID=manager-01
export GITEA_CODESPACE_ADMIN_LISTEN=127.0.0.1:18080
export GITEA_CODESPACE_ADMIN_TOKEN="$(openssl rand -hex 32)"
```

Multi-node deployments can use etcd for the same state objects:

```bash
export GITEA_CODESPACE_STATE=etcd
export GITEA_CODESPACE_ETCD_ENDPOINTS=http://127.0.0.1:2379
export GITEA_CODESPACE_ETCD_PREFIX=/gitea-codespace
export GITEA_CODESPACE_STATE_ENCRYPTION_KEY='<same key as the execution node>'
export GITEA_CODESPACE_NODE_ID=gateway-01
gitea-codespace gateway
```

`gitea-codespace serve` starts the administration server, worker, and gateway in one
process. `gitea-codespace gateway` starts only the HTTP and SSH
gateway and requires external etcd shared with a worker. It does not require an
administration token. This is useful when gateway nodes should be placed near the public
network edge while one elected leader owns lifecycle execution and shared capacity.
Other execution nodes watch a single leased etcd key and wait for leadership.
Each term recovers encrypted execution checkpoints from etcd; every candidate
must have access to the same Incus backends. No worker directory is transferred.
Gateway-only nodes connect to existing Incus projects without preparing resources
or recovering worker state. The cache registry runs on the leader; its configured
address must reach the current leader after failover.
All nodes must use the same state encryption key (reuse it, do not generate one
per node). The SSH host key is encrypted in etcd and shared by all gateways.

Start the service and open its administration page. The same process owns the
embedded store, management listener, workers, and gateway. HTTP Basic
authentication accepts the admin token as the password; the JSON site API uses
the same token as a Bearer credential.

```bash
./gitea-codespace serve
```

The administration listener serves HTTP. Keep it on loopback for local use, or
place it behind an authenticated TLS reverse proxy before exposing it to a
network. Both the administration token and newly entered Manager secrets are
credentials and must be encrypted in transit.

Create a Manager identity in the Gitea Codespace Manager settings page, then
enter its Gitea URL, Manager ID, and one-time secret in the administration page.
An enabled site is saved only after the Manager verifies that identity with
Gitea. A disabled site can be staged while Gitea is unavailable. Existing
secrets are never rendered; leaving the secret field empty keeps the encrypted
value already in state.

The runtime configuration is edited as YAML in the administration page and has
three top-level sections:

- `node` defines the local node name, capacity, and worker
  behavior.
- `gateway` defines the public HTTP and SSH entry points.
- `runtime` defines Git and Web IDE behavior, image caching, Incus backends,
  and the selectable runtime environments.

Each entry in `runtime.environments` declares a tag shown on the Codespace
creation page. It selects an Incus container or virtual-machine source and its
resource limits. See [`examples/config.example.yaml`](examples/config.example.yaml)
for all supported settings and deployment comments.

The page stores validated configuration in manager state. It is not a live
configuration file, and `serve` has no configuration-file flag. Site and runtime
configuration changes take effect together after the Manager process restarts.

### Storage lifecycle and backups

Embedded mode binds no etcd client, peer, or metrics listener. It uses the
official in-process client; the fixed peer URL is bootstrap metadata only.
The default data directory is `codespace-state/etcd`. Keep this directory
persistent and protected. A second process using the same
directory fails immediately. Empty or invalid runtime configuration leaves
the administration page available; save settings and restart to apply them.

Keep the encryption key and admin token persistent across restarts. Generate
them once and store them securely, rather than regenerating them at startup.
Nodes sharing external etcd must use the same encryption key and prefix.
External connections support `GITEA_CODESPACE_ETCD_CA`,
`GITEA_CODESPACE_ETCD_CERT` / `GITEA_CODESPACE_ETCD_KEY`, and
`GITEA_CODESPACE_ETCD_USERNAME` / `GITEA_CODESPACE_ETCD_PASSWORD`.
Certificate/key and username/password must be supplied together. Connections
with credentials require HTTPS and verify the server certificate.

Embedded etcd automatically compacts history older than one hour. Download a
snapshot through `GET /api/state/snapshot` with a Bearer admin token, for example:

```bash
curl --fail -H "Authorization: Bearer $GITEA_CODESPACE_ADMIN_TOKEN" \
  http://127.0.0.1:18080/api/state/snapshot -o manager-etcd.snapshot
```

Protect the downloaded file. Stop the Manager before restoring with the
matching etcdutl version to a new directory:

```bash
etcdutl snapshot status manager-etcd.snapshot
etcdutl snapshot restore manager-etcd.snapshot --name codespace --data-dir /var/lib/gitea-codespace/etcd-restored --initial-cluster codespace=http://127.0.0.1:2380 --initial-advertise-peer-urls http://127.0.0.1:2380 --initial-cluster-token etcd-cluster
```

Then set `GITEA_CODESPACE_STATE_PATH` to the restored directory and retain the
original encryption key. A disaster recovery also requires reconciliation
with Gitea and Incus; an etcd snapshot alone does not
back up runtime resources. External etcd backups and maintenance belong to
the cluster operator. Schedule defragmentation during maintenance: it can
block storage access and expire worker leases.

## Manager identity

Create a site-wide or personal manager in the corresponding Gitea Codespace
Manager settings, then generate connection credentials from its detail page.
Store the Gitea URL, manager ID, and secret through the local administration
page or API. The secret is encrypted before it is written to the state database.
Names can be edited without changing the identity. Resetting credentials keeps
the identity and instance bindings, but invalidates the old secret for subsequent
requests. Save the replacement secret and restart the affected Manager/Gateway
processes to apply it. Gitea shows the secret only in the generation or reset
response; credential replacement can interrupt control requests and lease renewals.

The etcd state database and its encryption key need to be protected and
persisted across restarts. The database stores site identities, runtime
configuration, ownership bindings, Gateway routing snapshots, the deployment
SSH host key, per-site lifecycle checkpoints, and inventory generations.
Gateways load a consistent
etcd routing snapshot and watch changes; reconnecting rebuilds that snapshot.

Leader loss cancels execution. Interrupted operations with unknown remote effects
wait for Gitea reconciliation instead of replaying initialization. Gateway activity
is published under leases; unavailable activity information pauses automatic stop.
Browser sessions remain local to each gateway, so use load-balancer session affinity
and authenticate again after a gateway failure.

Normal shutdown drains workers, metadata publishers, and network services before
closing the etcd client and embedded server. If execution cannot finish before
the shutdown deadline, the process stops renewing leadership and exits with an
error; the lease expires naturally instead of declaring a completed handover.
Registry blobs and files inside development instances remain on their respective
storage. They are not a second Manager recovery database.

Use a Gitea URL that is reachable from the manager and from the development
instances. A loopback URL normally refers to the instance itself and therefore
cannot be used by a Codespace to clone its repository.

## Run

After configuring the deployment, restart the same service to start the
manager and gateway. Run one process per data directory:

```bash
./gitea-codespace serve
```

The process creates one authenticated worker for each enabled site, declares the
configured environment tags, polls each Gitea for lifecycle operations,
reconciles site-owned Incus instances, and serves shared gateway listeners. All
site workers share the configured runtime and worker capacity, so adding a site
does not multiply the deployment limit.

The public gateway base domain, its `*.` wildcard, and the SSH address must
resolve to this deployment. A reverse proxy may terminate TLS for the HTTP
gateway, but it must preserve the original host and WebSocket connections used
by interactive endpoints.

## Architecture

- **Manager:** authenticates to Gitea, advertises environment tags, claims
  queued operations, and maintains isolated lifecycle state for each site.
- **Provisioner:** creates and controls Incus instances and executes commands or
  file operations through the Incus API.
- **Dev Container runtime:** resolves the repository configuration, builds or
  pulls its images, starts the development container, injects approved secrets,
  and launches the Web IDE.
- **Gateway:** authenticates Gitea-issued access and proxies Web IDE, endpoint,
  SSH, SFTP, and local port-forwarding sessions without exposing an instance
  directly. In etcd deployments, Gateway-only nodes read credential-free route
  snapshots and still authorize every connection against the owning Gitea.

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

The normal tests start real embedded etcd instances, including persistence,
official snapshot restore, worker leases, and a network-client test. These do
not require an installed etcd binary. To also validate a separately launched
server or a three-member cluster, use:

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

The Manager lifecycle targets (including the matrix) require
`CODESPACE_E2E_REPO_CLONE_HTTP_URL` and `CODESPACE_E2E_REPO_COMMIT_SHA`.
Use a repository with a `main` branch and a commit on that branch, accessible
from the Incus instance. The test clones that branch to exercise normal
workspace initialization before stopping and resuming the same instance.
`CODESPACE_E2E_INCUS_PROJECT` defaults to `default`.

Run the real Docker interoperability tests for the Dev Container implementation
with:

```bash
make test-devcontainer-e2e-required
```

## License

This project is licensed under the MIT License. See [`LICENSE`](LICENSE) for the
full text.
