# Develop a Web IDE provider

A Web IDE provider packages an editor and the small adapter that starts it
inside a Dev Container. The provider is an independent OCI image so editor
releases can be updated without rebuilding Manager, Agent, or Gateway.

This guide describes provider protocol version 1. The fake provider under
[`tests/e2e/web-ide`](../tests/e2e/web-ide) is a compact working reference; the
[`vscode`](vscode) and [`dsh`](dsh) directories show
production adapters.

## Lifecycle

Each Codespace retains the provider image reference and options selected at
creation. See the [provider overview](README.md) for template configuration,
image reference formats, pull policies, and when image updates take effect.

Creation and resume use the same startup sequence:

1. Kubernetes mounts the selected image's `/provider` directory read-only at
   `/opt/gitea-codespace/web-ide` in the Runtime using an `image` volume.
2. Agent validates the bundle manifest and mounts the bundle read-only in the
   Dev Container.
3. Agent invokes `bin/prepare` as the Dev Container remote user. The adapter
   receives the template and repository settings on standard input.
4. Agent starts `bin/serve` with a detached Docker execution and publishes the
   browser endpoint after Docker accepts that execution.

The image supplies files rather than running an editor process itself. The editor
runs inside the Dev Container, so its executable must be compatible with the
libraries available in the Dev Container images that the environment supports.
A statically linked executable is the most portable choice.

The failure stage determines which access remains available:

| Stage | Effect |
| --- | --- |
| Image pull or volume mount | Runtime startup waits for Kubernetes to mount the image; Dev Container access, including SSH, is not available yet |
| Agent bundle validation, `bin/prepare`, or Docker execution submission | Agent records a warning and withholds the Web IDE endpoint; Dev Container startup and SSH proceed independently |
| Editor startup or a later editor exit | The published endpoint remains present; Gateway returns a temporary `502 Workspace is unavailable` while the editor is not listening |

This separates the files required to start the Runtime from the editor process
running inside it. Provider process recovery is described under
[`bin/serve`](#binserve).

After a successful launch submission, Agent schedules `postAttachCommand` once
the required Dev Container lifecycle commands finish. A failure in that command
is logged while editor access remains available.

## Provider identity and image metadata

Choose a stable provider ID. It must start and end with a lowercase letter or
digit, may contain lowercase letters, digits, and hyphens, and may be at most 32
characters. The ID connects four parts of the contract:

- the OCI metadata;
- the bundle manifest;
- the environment-template configuration;
- the repository's Dev Container `customizations` object.

Every supported platform manifest must contain these OCI configuration labels:

| Label                                            | Meaning                                                |
| ------------------------------------------------ | ------------------------------------------------------ |
| `org.opencontainers.image.version`               | Provider or editor release, at most 128 characters     |
| `org.opencontainers.image.source`                | HTTP or HTTPS URL for the provider source              |
| `org.opencontainers.image.revision`              | Source revision used to build the image                |
| `dev.gitea.codespace.web-ide.metadata`           | Provider identity and environment option schema        |

The Codespace label is a JSON object. Save this example as `metadata.json` for
the image build below. It exposes two administrator options and supplies
defaults that Manager can place in a new environment:

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
KiB. During image resolution, Manager takes the schema's root default object
or constructs an object from property defaults, including nested objects, and
validates that result against the schema. Supply defaults that satisfy required
fields and other constraints so image resolution provides a valid starting
configuration. Manager also validates edited options before saving the template.

For a multi-platform image, the provider metadata, version, source, and
revision must be identical on `linux/amd64` and `linux/arm64`. This makes one
template behave the same regardless of the Kubernetes node selected for a pod.

## Bundle layout

The image contains this directory at `/provider`:

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

The manifest identifies the selected provider and architecture, uses schema
version `1`, and contains a nonempty version.

Make directories traversable and files readable by the Dev Container remote
user, and preserve executable permissions on programs. The image can use
`FROM scratch`: scripts use the Dev Container's interpreter and programs use
its system libraries. Writable settings, extensions, and editor state belong
in the separate persistent data directory. This separates cached program files
from user data and lets kubelet and the container runtime manage image storage.

The persistent data directory root belongs to the Dev Container's actual remote
user before adapters start, so images can retain their own user IDs. Existing
files retain their permissions across stop and resume. See the
[startup design](https://github.com/ExplodingDragon/gitea-dev/blob/main/src/codespace/runtime-platform.md#web-ide-交付与启动)
for lifecycle sequencing and failure handling.

## Adapter programs

Both programs run inside the Dev Container as its remote user and receive the
following environment:

| Variable                         | Value                                                   |
| -------------------------------- | ------------------------------------------------------- |
| `CODESPACE_WEB_IDE_PROVIDER`     | Stable provider ID                                      |
| `CODESPACE_WEB_IDE_PROGRAM_DIR`  | Read-only image bundle                                  |
| `CODESPACE_WEB_IDE_DATA_DIR`     | Persistent, writable provider data directory            |
| `CODESPACE_WEB_IDE_WORKSPACE`    | Repository workspace inside the Dev Container           |
| `CODESPACE_WEB_IDE_ADDRESS`      | Required loopback listen address in `host:port` form     |
| `CODESPACE_SSH_HOST`            | Public Gateway DNS name for this Runtime Pod             |
| `CODESPACE_SSH_PORT`            | Public SSH port as a decimal number                      |
| `CODESPACE_SSH_USER`            | Platform-assigned Gateway SSH username for the workspace |

The normal Dev Container remote environment and the secrets selected for this
instance are also available. Provider state belongs in `CODESPACE_WEB_IDE_DATA_DIR`.
The SSH variables let providers generate local connection instructions; their
refresh behavior and usage examples are documented in the [SSH guide](../docs/ssh.md).
Expose only these public fields to a browser interface; the complete process
environment also contains credentials.
Because the adapter intentionally receives the development environment and its
secrets, selecting a provider image is equivalent to trusting another
development tool. A tag trusts future content published under that tag; an
explicit digest keeps the reviewed program content stable and auditable.

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

A nonzero exit withholds the Web IDE endpoint for that start, as described in
the [lifecycle table](#lifecycle). Emit a concise error to standard error so it
appears in the codespace operation log.

### `bin/serve`

`bin/serve` owns the Provider process lifetime. It must remain in the foreground
and make the editor listen on exactly `CODESPACE_WEB_IDE_ADDRESS`. Binding to
this loopback address keeps access on the authenticated Gateway path instead of
exposing another pod listener.

```sh
#!/bin/sh
set -eu
exec "${CODESPACE_WEB_IDE_PROGRAM_DIR:?}/program/bin/example-ide" \
  --listen "${CODESPACE_WEB_IDE_ADDRESS:?}" \
  --workspace "${CODESPACE_WEB_IDE_WORKSPACE:?}" \
  --config "${CODESPACE_WEB_IDE_DATA_DIR:?}/config.json"
```

Replace the flags with the editor's own command line. The editor must serve HTTP
and WebSocket traffic from the same address. A simple Provider can use `exec` as
shown above. A Provider that needs automatic restart owns that behavior in a
foreground process manager behind `bin/serve`.

Endpoint publication and startup failures follow the [lifecycle table](#lifecycle),
so process recovery remains the provider's responsibility.

Providers can report unexpected exits through the [workspace event command](../docs/events.md).
Keep error details bounded and reporting failures independent of process
recovery. The platform accepts these events during startup and while the
instance runs, using the same Gitea log as lifecycle output.

## Build the carrier image

Build editor and adapter binaries outside the carrier image and arrange one
bundle per architecture:

```text
dist/
  <architecture>/
    manifest.json
    bin/prepare
    bin/serve
    program/bin/example-ide
    program/bin/example-adapter
Dockerfile
metadata.json
```

Use `amd64` and `arm64` as the architecture directory names. Before building,
set readable assets and executable programs with permissions suitable for any
remote user. A data-only Dockerfile is sufficient:

```dockerfile
FROM scratch

ARG TARGETARCH
ARG PROVIDER_VERSION
ARG PROVIDER_REVISION
ARG PROVIDER_METADATA

COPY dist/${TARGETARCH}/ /provider/

LABEL org.opencontainers.image.version=$PROVIDER_VERSION \
      org.opencontainers.image.source="https://example.com/owner/provider" \
      org.opencontainers.image.revision=$PROVIDER_REVISION \
      dev.gitea.codespace.web-ide.metadata="$PROVIDER_METADATA"
```

Publish a manifest that contains both supported Linux architectures, using the
same metadata file for their labels:

```sh
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg PROVIDER_VERSION=1.0.0 \
  --build-arg PROVIDER_REVISION="$(git rev-parse HEAD)" \
  --build-arg PROVIDER_METADATA="$(cat metadata.json)" \
  --tag registry.example.com/tools/example-web-ide:1.0.0 \
  --push .
```

Manager accepts a single-platform image as well, but a codespace can then run
only on nodes with that architecture. For private registries, configure
[image access](../charts/gitea-codespace/README.md#configure-web-ide-storage-and-image-access)
for both Manager metadata resolution and node image pulls.

## Test the image locally

Use a test cluster meeting the [deployment requirements](../charts/gitea-codespace/README.md#requirements).
After publishing the image, apply this temporary Pod with your image reference.
It verifies the same read-only subdirectory mount used by Codespace and reads
it as a non-root user:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: web-ide-bundle-check
spec:
  restartPolicy: Never
  containers:
    - name: check
      image: busybox:1.37.0
      command: [sh, -ec]
      args:
        - cat /bundle/manifest.json; test -x /bundle/bin/prepare; test -x /bundle/bin/serve
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
      volumeMounts:
        - name: program
          mountPath: /bundle
          subPath: provider
          readOnly: true
  volumes:
    - name: program
      image:
        reference: registry.example.com/tools/example-web-ide:1.0.0
        pullPolicy: Always
```

Using `Always` checks the current tag even when an earlier build is cached. For
an image imported into the node's container runtime, use its imported
`repository@sha256:...` reference with `IfNotPresent` to test that exact content
without requiring a provider registry request.

Inspect `kubectl logs web-ide-bundle-check` and the Pod's exit status, then remove
it with `kubectl delete pod web-ide-bundle-check`. Repeat on each supported node
architecture. For a private registry, supply the Pod's `imagePullSecrets` and
configure registry trust on the node.

Follow the project [connection guide](../README.md#connect-gitea) to select the
provider image in an environment template, then create a Codespace to validate
Agent's manifest checks and the adapter inside a compatible Dev Container.
Repeat with the supported isolation modes and verify that editor state survives
a stop and resume. Use the [lifecycle table](#lifecycle) to identify which startup
stage failed.

## Maintain the official providers

On `main`, each official provider publishes `latest` and its actual bundle
version to the same `linux/amd64` + `linux/arm64` index. Publishing verifies
both platform build records use the same version before creating the index.
PRs and manual runs on other branches validate without publishing. Image update
and pinning behavior is documented in the [provider overview](README.md).

The official providers use their `bin/serve` shell scripts to restart editors
after exit, with a delay to avoid a tight failure loop. Shutdown signals reach
the editor and end the loop so stopping the Dev Container does not start another
process.

Each provider has an independent toolchain because its application and
integration follow upstream dependencies. Select Node.js from that provider's
`.node-version` file and pnpm from its `provider-<name>.yml` workflow.

From the repository root, validate the provider being changed:

```sh
make -C providers/vscode check
make -C providers/dsh check
```

Update upstream versions in the provider's `version.env`. For DSH,
keep Node archive checksums aligned with `.node-version` so verified downloads
match the selected toolchain. VS Code also accepts `CODE_SERVER_VERSION` for a
different code-server release.

`make -C providers/<provider> version` reports the configured editor version.
Each provider's `image` target builds for the current architecture and uses the
completed bundle manifest for its version label. Build on the target architecture
because dependencies include native code. For example, from the repository root:

```sh
make -C providers/dsh image IMAGE=registry.example.com/dsh:test
```

Use the [local image verification steps](#test-the-image-locally) to check the
mounted bundle. The [VS Code](vscode/README.md) and [DSH](dsh/README.md)
guides describe user configuration;
their Makefiles define the checks and build steps for each toolchain.

Each provider workflow runs for changes to its own source. Changes to the shared
[build workflow](../.github/workflows/provider-build.yml) trigger all providers;
the [platform workflow](../.github/workflows/ci.yml) handles core changes separately.
