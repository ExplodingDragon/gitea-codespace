# Gitea Codespace

Gitea Codespace provides remote development workspaces for Gitea repositories.
Each workspace is built from the repository's Dev Container configuration and
can be opened in a browser IDE, a desktop editor, or over SSH and SFTP.

## Use a codespace

After an administrator connects Gitea to Codespace:

1. Open a repository, commit, or pull request in Gitea.
2. Select **Create Codespace**, then choose a Dev Container configuration and
   an environment.
3. Open the workspace after the build completes.

Workspace files remain available after a codespace is stopped. Published
services and their access addresses appear on the codespace details page.

## Deploy

Cluster administrators can follow the [Helm guide](charts/gitea-codespace/README.md)
to prepare Kubernetes, install Codespace, connect Gitea, and publish workspace
access.

## Contribute

[DEVELOPMENT.md](DEVELOPMENT.md) describes the repository layout and validation
workflow.

## License

[MIT](LICENSE)
