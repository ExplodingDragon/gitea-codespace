# Gitea Codespace

Gitea Codespace provides remote development environments from a repository's
Dev Container configuration. Workspaces are available in a browser, a desktop
editor, or through SSH and SFTP.

## Use a codespace

After an administrator connects Gitea to Codespace:

1. Open a repository, commit, or pull request.
2. Select **Create Codespace**.
3. Choose a Dev Container configuration and environment.
4. Open the workspace after the build completes.

The details page contains the available editor, SSH, and application links.
Stopping a codespace preserves its workspace for the next start.

## Install and operate

Use the [Helm guide](charts/gitea-codespace/README.md) to prepare Kubernetes,
install Codespace, connect Gitea, and publish workspace access.

## Contribute

See [DEVELOPMENT.md](DEVELOPMENT.md) for the contributor workflow.

## License

[MIT](LICENSE)
