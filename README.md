# Gitea Codespace

Gitea Codespace provides isolated remote development workspaces for Gitea
repositories. Each workspace follows the repository's Dev Container
configuration and is available from a browser IDE, desktop editor, SSH, or
SFTP.

## Use Codespaces

After an administrator connects Gitea to a Codespace installation:

1. Open a repository, commit, or pull request in Gitea.
2. Select **Create Codespace** and choose the development environment.
3. Open the workspace when its build completes.

A stopped codespace keeps its workspace files and can be resumed later. Delete
it when those files are no longer needed.

Applications exposed with `gitea-codespace-endpoint` appear on the Codespace
details page. An endpoint can require Gitea authentication or allow public
access.

## Install

See the [Helm guide](charts/gitea-codespace/README.md) for cluster requirements,
installation, upgrades, and Gitea registration.

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) for repository setup and validation.

## License

[MIT](LICENSE)
