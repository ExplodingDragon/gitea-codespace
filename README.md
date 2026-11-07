# Gitea Codespace

Gitea Codespace provides isolated, Dev Container-based workspaces for Gitea
repositories. A workspace can be used from a browser IDE, desktop editor, SSH,
or SFTP.

## Create a workspace

After an administrator enables Codespaces for the Gitea instance:

1. Open a repository, commit, or pull request in Gitea.
2. Select **Create Codespace** and choose an environment and Dev Container
   configuration.
3. Wait for the build to finish, then choose one of the displayed connection
   options.

A stopped Codespace keeps its workspace and can be resumed later. Delete it
when the workspace is no longer needed.

## Publish an application

Use `gitea-codespace-endpoint` inside the Dev Container to publish an HTTP
service. An endpoint can require the current user's Gitea access or be published
for public access.

## Install

Cluster administrators can install Gitea Codespace with its OCI Helm chart. The
[Helm guide](charts/gitea-codespace/README.md) covers cluster requirements,
installation, and initial configuration.

## More information

- [Helm installation and operations](charts/gitea-codespace/README.md)
- [Contributor guide](DEVELOPMENT.md)
- [Architecture](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)

## License

[MIT](LICENSE)
