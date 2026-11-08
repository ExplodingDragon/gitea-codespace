# Gitea Codespace

Gitea Codespace creates remote development workspaces from a repository's Dev
Container configuration. A codespace can be opened in the browser, from a
desktop editor, or over SSH and SFTP.

## Create a codespace

After an administrator connects Gitea to Codespace:

1. Open a repository, commit, or pull request.
2. Select **Create Codespace**.
3. Choose a Dev Container configuration and environment.
4. Open the workspace when the build completes.

Stopping a codespace preserves its workspace. The details page provides editor,
SSH, and published service access when the codespace is running.

## Administration

Use the [Helm guide](charts/gitea-codespace/README.md) to prepare Kubernetes,
install Codespace, connect Gitea, and publish workspace access.

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) for the contributor workflow.

## License

[MIT](LICENSE)
