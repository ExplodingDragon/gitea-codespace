# Develop a Web IDE provider

A Web IDE provider packages an editor and the small adapter that starts it
inside a Dev Container. The provider is an independent OCI image so editor
releases can be updated without rebuilding Manager, Agent, or Gateway.

This guide describes provider protocol version 1. The fake provider under
[`tests/e2e/web-ide`](../tests/e2e/web-ide) is a compact working reference; the
[`vscode`](vscode) and [`dsh`](dsh) directories show production adapters.

## Lifecycle

When creating a template with Web IDE enabled or changing its enabled Web IDE
configuration, Manager validates the provider's OCI metadata and options, then
records the selected tag or explicit `sha256` digest. Other template edits retain
the validated Web IDE configuration so they can be saved independently of registry
availability. Image names use [Docker's reference rules](README.md), with
`latest` as the default tag; when both a tag and digest are present, the digest
selects the content. A new Codespace copies that configuration into its runtime
snapshot; later template edits apply to subsequently created Codespaces.

New or replacement Pods pull tagged images with `Always`, including during
resume. Explicit digests use `IfNotPresent`. A running Pod keeps its mounted
content; use a digest to keep the same program across Pod replacements. Node
image caches reuse layers with matching digests under either policy.

Each start uses that snapshot:

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

### Implementation acceptance criteria

- Saving an environment preserves its normalized tag or explicit digest.
- Creating a template with Web IDE enabled or changing its enabled Web IDE
  configuration validates the image metadata and options; unrelated edits
  preserve that configuration without a registry request.
- New Pods refresh tags and reuse fixed digests; running Pods retain their image.
- Starting a codespace mounts the selected bundle, prepares it as the remote
  user, starts `bin/serve`, and publishes one browser endpoint.
- The editor process runs inside the Dev Container and can use the workspace,
  declared environment, and persistent provider data.
- An image-volume failure blocks Runtime startup; a later editor preparation
  or launch failure only withholds Web IDE access.
- Creation and resume use the same startup sequence. After a successful launch,
  `postAttachCommand` runs; a command failure is logged while editor access remains
  available.

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
fields and other constraints: invalid defaults fail image resolution before the
administrator can edit the options. This gives each resolved image a valid
starting configuration. Manager also validates edited options before saving
the environment template.

For a multi-platform image, the provider metadata, version, source, and
revision must be identical on `linux/amd64` and `linux/arm64`. This makes one
template behave the same regardless of the Kubernetes node selected for a pod.

### Implementation acceptance criteria

- Manager resolves the image, displays its provider identity and options, and
  stores the normalized image reference in the environment template.
- Valid option values round-trip through the administration form and validate
  against the embedded schema.
- Resolving the image produces default options that already satisfy its schema;
  administrators can then edit and save those options. Re-resolving the same
  provider preserves edited options; selecting a different provider uses its defaults.
- Every platform manifest presents the same provider ID, version, source,
  revision, and option schema.

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

Kubernetes selects the node architecture and mounts the image with
`subPath: provider`. The manifest must identify the selected provider and
architecture, use the supported schema version, and contain a nonempty version.

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

### Implementation acceptance criteria

- Mounting the image's `provider` subdirectory exposes the complete bundle.
- The manifest matches the selected provider and architecture and declares a
  supported schema version and nonempty version.
- The remote user can read the manifest and assets and execute both adapters
  from the read-only bundle mounted in the Dev Container.
- On creation and resume, the remote user can write the data directory even
  when its UID differs from the Runtime host user.

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
| `CODESPACE_SSH_USER`            | Gateway login for the workspace: `cs-<runtime UUID>`      |

The normal Dev Container remote environment and declared secrets are also
available. Provider state belongs in `CODESPACE_WEB_IDE_DATA_DIR`; the program
bundle is read-only and comes from the image reference in the Codespace runtime
snapshot, following the pull policy described in [Lifecycle](#lifecycle).
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

Docker accepting the detached execution completes the platform launch contract.
Once the endpoint is published, later requests can reach a restarted editor
without changing the Codespace lifecycle state.

### Implementation acceptance criteria

- `bin/prepare` accepts both initialization values, combines template and
  repository options deterministically, and stores writable state only in the
  provider data directory.
- Adapter processes receive the current Pod's SSH connection information;
  browser connection APIs return only the public user, host, and port.
- A valid bundle whose `prepare` program succeeds receives a browser endpoint
  as soon as Docker accepts its detached `bin/serve` execution; editor listen
  timing does not determine Endpoint publication or Codespace lifecycle state.
- Requests made before the editor listens receive the temporary Gateway error;
  requests made after it listens succeed without resuming the codespace.
- Exiting `bin/serve` does not stop SSH, the Dev Container, or other endpoints.
- A Provider that needs automatic restart keeps its own process manager in the
  foreground behind `bin/serve`.

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

### Implementation acceptance criteria

- Each platform image contains `/provider` with the matching manifest and
  executable adapters, accessible to a non-root remote user.
- Published platform manifests share the same metadata and resolve successfully
  in Manager.

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
it with `kubectl delete pod web-ide-bundle-check`. For a private registry, supply
the Pod's `imagePullSecrets` and configure registry trust on the node.

Create a Codespace to also validate Agent's manifest checks and the adapter
inside a compatible Dev Container. Repeat with the supported isolation modes
and verify that editor state survives a stop and resume. Use the
[lifecycle table](#lifecycle) to identify which startup stage failed.

### Implementation acceptance criteria

- The test Pod completes and reads the expected manifest on every supported
  node architecture.
- Recreating the test Pod with a published tag checks its current image;
  an imported digest selects the exact locally available image.
- A Codespace prepares and serves the editor as its remote user from the
  read-only bundle, and retains provider data across stop and resume.

## Connect the provider to Codespace

1. Open **Environment templates** in the administration panel.
2. Enter a registry image with an optional tag or explicit digest and select
   **Resolve**. An omitted tag defaults to `latest`.
3. Review the provider name, version, platforms, and generated default options.
4. Save the environment template. Manager validates the options and preserves
   the normalized image reference.
5. Create a codespace with that environment and open its browser workspace.

Publishing to an existing tag makes the update available to new or replacement
Pods using that tag. Resolve the image again to inspect its current metadata;
the displayed version describes that lookup, while the saved tag can change
later. To select a different reference or options, edit and save the template.
The [lifecycle](#lifecycle) snapshot rules determine which Codespaces use it.

### Implementation acceptance criteria

- A newly created Codespace uses the saved image reference and configuration.
- Updating a template leaves existing Codespace snapshots unchanged.

## Maintain the official providers

On `main`, each official provider publishes `latest` and its actual bundle
version to the same `linux/amd64` + `linux/arm64` index. Publishing verifies
both platform build records use the same version before creating the index.
PRs and manual runs on other branches validate without publishing. Version tags
can receive adapter or plugin fixes for the same editor version; use an explicit
digest to select exact content independently of those updates.

The official VS Code and DeepSeek Harness providers restart their editor after
every exit, with a one-second delay to avoid a tight failure loop. They restart
after both successful and failed exits because their role is to keep the editor
running. `HUP`, `INT`, and `TERM` are forwarded to the current editor process and
end the provider loop, which lets Docker stop the Dev Container without the
provider starting a replacement process.

The VS Code and DSH projects have independent toolchains because their editor
and plugin builds follow different upstream dependencies. Select Node.js from
the project's [VS Code](vscode/.node-version) or [DSH](dsh/.node-version) version
file, and pnpm from its [VS Code](../.github/workflows/provider-vscode.yml) or
[DSH](../.github/workflows/provider-dsh.yml) workflow.

From the repository root, validate the provider being changed:

```sh
make -C providers/vscode check
make -C providers/dsh check
```

The DSH check fetches its pinned upstream sources, type-checks and lints the
Codespace plugin, then bundles and tests it against that version. Its
[version file](dsh/version.env) pins the source release,
commit, pnpm version, and Node archive checksums; update the checksums with
[`.node-version`](dsh/.node-version) so the toolchain and verified downloads
stay consistent. VS Code uses its [version file](vscode/version.env) as the
default and accepts `CODE_SERVER_VERSION` for a different code-server release.
`make version` reports the configured editor version. Each provider's `image`
target builds for the current architecture; local builds and CI read the image
version label from the completed bundle manifest so it describes the packaged
release. Use the [local image verification steps](#test-the-image-locally) to
check its mounted bundle.

Each provider workflow runs for changes to its own source. Changes to the shared
[build workflow](../.github/workflows/provider-build.yml) trigger both providers;
the [platform workflow](../.github/workflows/ci.yml) handles core changes separately.

### Implementation acceptance criteria

- Both official providers restart the editor after a successful or failed exit,
  with a one-second delay between attempts.
- `HUP`, `INT`, and `TERM` reach the running editor and end its restart loop.
- Each provider passes its own `check` target and supplies a valid bundle for
  every published architecture.
- Image version labels match the completed bundle manifest, including when
  building VS Code with a version override.
- Both platform records have the same version before publishing, and `latest`
  and the bundle-version tag resolve to the same multi-platform index.
