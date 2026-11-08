# Gitea Codespace

Gitea Codespace provides remote development workspaces for Gitea repositories.
It uses the repository's Dev Container configuration and supports browser IDEs,
desktop editors, SSH, and SFTP.

## Get started

After an administrator connects Gitea to Codespace:

1. Open a repository, commit, or pull request.
2. Select **Create Codespace** and choose an environment.
3. Open the workspace after the build completes.

Stopping a codespace preserves its workspace files for the next session.
Services published from the workspace appear on its details page with their
configured access policy.

## Install and operate

The [Helm guide](charts/gitea-codespace/README.md) covers cluster requirements,
installation, Gitea registration, workspace access, and upgrades.

## Contribute

[DEVELOPMENT.md](DEVELOPMENT.md) describes the repository layout and validation
workflow.

## License

[MIT](LICENSE)
