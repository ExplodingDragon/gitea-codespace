# DeepSeek Harness Web IDE provider

This project packages DeepSeek Harness as the official `dsh` provider. Its
small trusted-proxy patch lets the process rely on Codespace Gateway browser
authentication while remaining bound to the Dev Container loopback address.
The patch and upstream build remain in this provider so the security boundary
is reviewable, while Manager and Agent use the same Provider contract as every
other Web IDE.

The provider accepts `plugins` and `patches` options. Values declared by the
environment template are followed by repository `customizations.dsh` values and
are written to the provider's persistent profile during initial preparation.

Run `make -C providers/dsh check` for adapter and script checks. Run
`make -C providers/dsh image` on a host matching the requested architecture to
build the pinned upstream release and a local provider image. The dedicated
GitHub workflow uses native amd64 and arm64 runners and publishes the combined
image as `ghcr.io/explodingdragon/gitea-codespace-web-ide-dsh:main`.
