# Gitea Codespace

Gitea Codespace provides isolated, Dev Container-based workspaces for Gitea
repositories. A workspace can be used from a browser IDE, desktop editor, SSH,
or SFTP.

## Use a codespace

After an administrator enables Codespaces for the Gitea instance:

1. Open a repository, commit, or pull request in Gitea.
2. Select **Create Codespace** and choose an environment and Dev Container
   configuration.
3. Wait for the build to finish and open one of the available connections.

A stopped codespace retains its files and can be resumed later. Delete the
codespace when its workspace is no longer needed.

Use `gitea-codespace-endpoint` inside the Dev Container to expose an HTTP
service. It then appears under **Ports** and can require Gitea authentication or
allow public access.

## Install

Cluster administrators install Gitea Codespace with its OCI Helm chart. See the
[Helm guide](charts/gitea-codespace/README.md) for runtime requirements,
installation, and initial configuration.

## Contribute

See the [contributor guide](DEVELOPMENT.md) for the local toolchain, common
commands, generated sources, and Kubernetes testing workflow.

## License

[MIT](LICENSE)
