# Gitea Codespace

Gitea Codespace provides isolated, Dev Container-based development environments
for Gitea repositories. Create and manage a Codespace from Gitea, then work in a
browser IDE or connect with a desktop editor, SSH, or SFTP.

## Use a Codespace

After an administrator enables Codespaces for the Gitea instance:

1. Open a repository, commit, or pull request.
2. Select **Create Codespace**, then choose an environment and Dev Container
   configuration available to you.
3. Follow the build log until the environment is ready.
4. Open the workspace with one of the displayed connection options.

A stopped Codespace retains its workspace and can be resumed without cloning the
repository again. Delete it when the workspace is no longer needed.

## Connect to applications

Use `gitea-codespace-endpoint` inside the Dev Container to publish an HTTP
service. Private endpoints require the current user's Gitea access; public
access is enabled explicitly for an endpoint.

## Deploy

Gitea Codespace runs on Kubernetes and is distributed as an OCI Helm chart. See
the [Helm guide](charts/gitea-codespace/README.md) for requirements,
installation, and initial configuration.

## Documentation

- [Helm installation and operations](charts/gitea-codespace/README.md)
- [Contributor guide](DEVELOPMENT.md)
- [Architecture](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)

## License

[MIT](LICENSE)
