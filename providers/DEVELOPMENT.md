# Develop a Web IDE provider

A Web IDE provider packages an editor and the small adapter that starts it
inside a Dev Container. The provider is an independent OCI image so editor
releases can be updated without rebuilding Manager, Agent, or Gateway. Manager
reads the image metadata when an administrator saves an environment template,
then records the resolved image digest so later starts use the same reviewed
content.

This guide describes provider protocol version 1. The fake provider under
[`tests/e2e/web-ide`](../tests/e2e/web-ide) is a compact working reference; the
[`vscode`](vscode) and [`dsh`](dsh) directories show production adapters.

## Lifecycle

The selected provider participates in a codespace start as follows:

1. Manager resolves the configured image tag, validates its OCI metadata, and
   stores an immutable `sha256` reference in the environment template.
2. Kubernetes runs the image once as an init container. Its entrypoint copies
   the provider bundle to `/output/current`.
3. Agent validates the bundle manifest and mounts the bundle read-only in the
   Dev Container.
4. Agent invokes `bin/prepare` as the Dev Container remote user. The adapter
   receives the template and repository settings on standard input.
5. Agent starts the `bin/serve` supervisor and publishes the browser endpoint
   as soon as the supervisor process starts successfully.

The init image is a carrier rather than the editor process itself. The editor
runs inside the Dev Container, so its executable must be compatible with the
libraries available in the Dev Container images that the environment supports.
A statically linked executable is the most portable choice.

## Provider identity and image metadata

Choose a stable provider ID. It must start and end with a lowercase letter or
digit, may contain lowercase letters, digits, and hyphens, and may be at most 32
characters. The ID connects four parts of the contract:

- the OCI metadata;
- the exported bundle manifest;
- the environment-template configuration;
- the repository's Dev Container `customizations` object.

Every supported platform manifest must contain these OCI configuration labels:

| Label                                            | Meaning                                                |
| ------------------------------------------------ | ------------------------------------------------------ |
| `org.opencontainers.image.version`               | Provider or editor release, at most 128 characters     |
| `org.opencontainers.image.source`                | HTTP or HTTPS URL for the provider source              |
| `org.opencontainers.image.revision`              | Source revision used to build the image                |
| `dev.gitea.codespace.web-ide.metadata`           | Provider identity and environment option schema        |

The Codespace label is a JSON object. This example exposes two administrator
options and supplies defaults that Manager can place in a new environment:

```json
{
  "metadataVersion": 1,
  "protocolVersion": 1,
  "provider": {
    "id": "example",
    "name": "Example IDE"
  },
  "optionsSchema": {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "type": "object",
    "properties": {
      "extensions": {
        "type": "array",
        "items": {"type": "string", "minLength": 1},
        "uniqueItems": true,
        "default": []
      },
      "settings": {
        "type": "object",
        "additionalProperties": true,
        "default": {}
      }
    },
    "additionalProperties": false
  }
}
```

Manager accepts a Draft 2020-12 schema whose root type is `object`. Schema
references stay inside the label by using local `#...` references. The complete
metadata label is limited to 32 KiB, and saved option values are limited to 16
KiB. Manager validates environment-template options against this schema before
saving them.

For a multi-platform image, the provider metadata, version, source, and
revision must be identical on `linux/amd64` and `linux/arm64`. This makes one
template behave the same regardless of the Kubernetes node selected for a pod.

## Bundle layout

The image entrypoint publishes this directory at `/output/current`:

```text
manifest.json
bin/prepare
bin/serve
program/...
```

`manifest.json` describes the platform-specific bundle:

```json
{
  "schemaVersion": 1,
  "provider": "example",
  "architecture": "amd64",
  "version": "1.0.0"
}
```

`architecture` is `amd64` or `arm64` and must match the image being run.
The manifest is limited to 64 KiB. `bin/prepare` and `bin/serve` must be regular
executable files. Everything else under the bundle is provider-owned.

The entrypoint should stage the complete bundle and finish with one rename. The
rename prevents Agent from observing a partially copied editor when the init
container is interrupted:

```sh
#!/bin/sh
set -eu

output=/output
expected=${CODESPACE_WEB_IDE_EXPECTED_PROVIDER:?}
staging="$output/.provider-$$"

test "$expected" = example
test ! -e "$output/current"
trap 'rm -rf "$staging"' EXIT INT TERM
mkdir -p "$staging"
cp -a /provider/. "$staging/"
test -x "$staging/bin/prepare"
test -x "$staging/bin/serve"
grep -Fq '"provider":"example"' "$staging/manifest.json"
mv "$staging" "$output/current"
trap - EXIT INT TERM
```

## Adapter programs

Both programs run inside the Dev Container as its remote user and receive the
following environment:

| Variable                         | Value                                                   |
| -------------------------------- | ------------------------------------------------------- |
| `CODESPACE_WEB_IDE_PROVIDER`     | Stable provider ID                                      |
| `CODESPACE_WEB_IDE_PROGRAM_DIR`  | Read-only exported bundle                               |
| `CODESPACE_WEB_IDE_DATA_DIR`     | Persistent, writable provider data directory            |
| `CODESPACE_WEB_IDE_WORKSPACE`    | Repository workspace inside the Dev Container           |
| `CODESPACE_WEB_IDE_ADDRESS`      | Required loopback listen address in `host:port` form     |

The normal Dev Container remote environment and declared secrets are also
available. Provider state belongs in `CODESPACE_WEB_IDE_DATA_DIR`; the program
bundle is read-only and is replaced when a template selects a new image digest.
Because the adapter intentionally receives the development environment and its
secrets, selecting a provider image is equivalent to trusting another
development tool. Digest pinning makes that reviewed input stable and
auditable.

### `bin/prepare`

Agent sends one JSON document to standard input before each editor start:

```json
{
  "protocolVersion": 1,
  "initialize": true,
  "templateOptions": {
    "extensions": ["publisher.extension"]
  },
  "repositoryOptions": {
    "extensions": ["repository.extension"]
  }
}
```

`initialize` is `true` while creating the Dev Container and `false` when
starting an existing one. `templateOptions` came from the administrator and was
validated against the image schema. `repositoryOptions` comes from the
repository's standard Dev Container customization block:

```jsonc
{
  "customizations": {
    "example": {
      "extensions": ["repository.extension"]
    }
  }
}
```

The adapter owns the merge policy because different editors have different
configuration formats. It should validate repository values, combine them
deterministically with template values, and atomically write the resulting
configuration under `CODESPACE_WEB_IDE_DATA_DIR`. The official providers apply
template values first and let repository values add to or override them.

A thin launcher keeps provider-specific parsing out of the platform:

```sh
#!/bin/sh
set -eu
exec "${CODESPACE_WEB_IDE_PROGRAM_DIR:?}/program/bin/example-adapter" prepare
```

A nonzero exit makes the Web IDE unavailable for that start, while SSH and the
rest of the codespace remain available. Emit a concise error to standard error
so it appears in the codespace operation log.

### `bin/serve`

`bin/serve` must remain in the foreground and make the editor listen on exactly
`CODESPACE_WEB_IDE_ADDRESS`. Binding to this loopback address keeps access on
the authenticated Gateway path instead of exposing another pod listener.

```sh
#!/bin/sh
set -eu
exec "${CODESPACE_WEB_IDE_PROGRAM_DIR:?}/program/bin/example-ide" \
  --listen "${CODESPACE_WEB_IDE_ADDRESS:?}" \
  --workspace "${CODESPACE_WEB_IDE_WORKSPACE:?}" \
  --config "${CODESPACE_WEB_IDE_DATA_DIR:?}/config.json"
```

Replace the flags with the editor's own command line. The editor must serve HTTP
and WebSocket traffic from the same address. The supervisor restarts the process
with bounded delay when it exits. Gateway may return `502 Workspace is
unavailable` before the process begins listening; a later request succeeds once
the provider is ready. Publishing the endpoint at supervisor startup avoids a
fixed startup deadline and lets the same restart path recover both slow starts
and later provider failures without changing the codespace lifecycle state.

### Implementation acceptance criteria

- A valid bundle whose `prepare` program succeeds receives a browser endpoint
  after the supervisor starts, even when the editor takes more than 30 seconds
  to listen.
- Requests made before the editor listens receive the temporary Gateway error;
  requests made after it listens succeed without resuming the codespace.
- Exiting `bin/serve` is restarted by the supervisor and does not stop SSH or the
  Dev Container.

## Build the carrier image

Build editor and adapter binaries outside the carrier image and arrange one
bundle per architecture:

```text
dist/
  amd64/
    manifest.json
    bin/prepare
    bin/serve
    program/bin/example-ide
    program/bin/example-adapter
  arm64/
    manifest.json
    bin/prepare
    bin/serve
    program/bin/example-ide
    program/bin/example-adapter
Dockerfile
export.sh
```

The carrier needs only file-copying tools. A small Dockerfile is sufficient:

```dockerfile
FROM busybox:1.37.0

ARG TARGETARCH
ARG PROVIDER_VERSION
ARG PROVIDER_REVISION

COPY --chmod=0555 export.sh /usr/local/bin/export-web-ide
COPY dist/${TARGETARCH}/ /provider/

LABEL org.opencontainers.image.version=$PROVIDER_VERSION \
      org.opencontainers.image.source="https://example.com/owner/provider" \
      org.opencontainers.image.revision=$PROVIDER_REVISION \
      dev.gitea.codespace.web-ide.metadata='{"metadataVersion":1,"protocolVersion":1,"provider":{"id":"example","name":"Example IDE"},"optionsSchema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{},"additionalProperties":false}}'

USER 0:0
ENTRYPOINT ["/usr/local/bin/export-web-ide"]
```

Publish a manifest that contains both supported Linux architectures:

```sh
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg PROVIDER_VERSION=1.0.0 \
  --build-arg PROVIDER_REVISION="$(git rev-parse HEAD)" \
  --tag registry.example.com/tools/example-web-ide:1.0.0 \
  --push .
```

Manager accepts a single-platform image as well, but a codespace can then run
only on nodes with that architecture. Private registries use the pull Secrets
configured for the Codespace Helm installation; credentials are not stored in
an environment template.

## Validate and publish an official provider

Each official provider has a focused `check` target that validates its scripts,
adapter, and provider-owned client code without compiling the Codespace
platform:

```sh
make -C providers/vscode check
make -C providers/dsh check
```

Pull requests that change an official provider run its dedicated validation
workflow. A push to `main` builds native amd64 and arm64 bundles and publishes a
multi-platform `main` image. The platform workflow ignores `providers/**`
because provider images have independent dependencies and release lifecycles;
changes that also touch platform files still run the platform workflow. Pure
documentation changes do not rebuild or publish an image.

The reusable `.github/workflows/provider-build.yml` workflow contains the
shared build and publication steps. Keeping provider-specific dependencies in
each provider directory prevents an editor update from expanding the platform
image or its build toolchain.

## Test the image locally

Run the init-container entrypoint with an empty output directory before
publishing. The command below uses the host architecture and leaves the
exported bundle available for inspection:

```sh
image=registry.example.com/tools/example-web-ide:1.0.0
architecture=$(go env GOARCH)
output=$(mktemp -d)

docker run --rm \
  --platform "linux/$architecture" \
  --env CODESPACE_WEB_IDE_EXPECTED_PROVIDER=example \
  --volume "$output:/output" \
  "$image"

test -f "$output/current/manifest.json"
test -x "$output/current/bin/prepare"
test -x "$output/current/bin/serve"
cat "$output/current/manifest.json"
```

An export failure prevents the runtime pod from starting because Kubernetes
must complete the init container first. A later `prepare` failure is reported
in the codespace operation log and only withholds browser access. A `serve`
failure is recorded in the supervisor log and retried; SSH and the rest of the
Dev Container remain available in both cases.

## Connect the provider to Codespace

1. Open **Environment templates** in the administration panel.
2. Enter the provider image with an explicit tag or digest and select
   **Resolve**.
3. Review the provider name, version, platforms, and generated default options.
4. Save the environment template. Manager stores the resolved digest rather
   than the mutable tag.
5. Create a codespace with that environment and open its browser workspace.

When publishing an updated provider, publish a new image, resolve it again in
the environment template, and save the template. Existing runtime snapshots
keep their selected digest until their normal lifecycle applies the updated
template.

## Verification checklist

Before publishing a provider for users, verify all of the following:

- `linux/amd64` and `linux/arm64` images carry identical provider metadata and
  standard OCI labels.
- Running the image with an empty directory mounted at `/output` creates only a
  complete `/output/current` bundle.
- The bundle manifest identity and architecture match the image.
- `bin/prepare` accepts both initialization values, validates repository
  options, and preserves state in the provider data directory.
- `bin/serve` listens on the supplied loopback address, serves HTTP, upgrades
  WebSocket connections, and stays in the foreground.
- Stopping and starting the editor process does not damage persistent provider
  state.
- Manager resolves the published reference and saves it as a `sha256` digest.

The repository's fake provider demonstrates these checks without depending on
a production editor release:

```sh
make e2e-web-ide-image
```
