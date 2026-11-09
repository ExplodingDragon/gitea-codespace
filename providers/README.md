# Web IDE providers

Web IDEs are distributed independently from the Codespace platform image. This
keeps editor dependencies and release schedules isolated while giving every
provider the same runtime contract.

## Official images

Gitea Codespace publishes:

- `ghcr.io/explodingdragon/gitea-codespace-web-ide-vscode:main`
- `ghcr.io/explodingdragon/gitea-codespace-web-ide-dsh:main`

Manager suggests these images in environment templates and also accepts a
compatible third-party image. It resolves the selected reference to an
immutable digest so an existing template continues using the reviewed image.

## Image metadata

Manager discovers a provider from OCI image configuration labels. Every
supported platform manifest declares these standard labels:

- `org.opencontainers.image.version`
- `org.opencontainers.image.source`
- `org.opencontainers.image.revision`
- `dev.gitea.codespace.web-ide.metadata`

The Codespace label is a JSON object containing the provider identity and its
environment-template options:

```json
{
  "metadataVersion": 1,
  "protocolVersion": 1,
  "provider": { "id": "example", "name": "Example IDE" },
  "optionsSchema": {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "type": "object",
    "properties": {},
    "additionalProperties": false
  }
}
```

The provider ID is a lowercase identifier of at most 32 characters. The inline
Draft 2020-12 Schema uses local references and describes all accepted options.
The amd64 and arm64 manifests of a multi-platform image declare identical
provider metadata and source information so a template has the same meaning on
every supported node.

## Runtime bundle

Kubernetes runs the provider image as an init container. With no arguments, its
entrypoint publishes a complete bundle at `/output/current`:

```text
manifest.json
bin/prepare
bin/serve
... provider-owned programs and assets
```

`manifest.json` contains `schemaVersion`, `provider`, `architecture`, and
`version`. The entrypoint checks `CODESPACE_WEB_IDE_EXPECTED_PROVIDER` and
publishes the bundle with one final rename so Agent only observes complete
content.

Agent mounts the bundle read-only in the Dev Container. Both programs receive:

- `CODESPACE_WEB_IDE_PROVIDER`
- `CODESPACE_WEB_IDE_PROGRAM_DIR`
- `CODESPACE_WEB_IDE_DATA_DIR`
- `CODESPACE_WEB_IDE_WORKSPACE`
- `CODESPACE_WEB_IDE_ADDRESS`

`bin/prepare` reads a JSON object from standard input containing
`protocolVersion`, `initialize`, `templateOptions`, and `repositoryOptions`. It
validates and merges provider-specific settings and writes persistent state to
`CODESPACE_WEB_IDE_DATA_DIR`.

`bin/serve` starts the editor on the loopback address in
`CODESPACE_WEB_IDE_ADDRESS`. Agent supervises the process and publishes browser
access after the address accepts connections.

## Develop a provider

Use [vscode](vscode/README.md) and [dsh](dsh/README.md) as complete examples.
The deterministic implementation under `tests/e2e/web-ide` is a smaller example
for contract testing.

Run the focused checks from the repository root:

```sh
make -C providers/vscode check
make -C providers/dsh check
```
