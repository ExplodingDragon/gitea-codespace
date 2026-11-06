# Gitea Codespace

Gitea Codespace provides isolated, Dev Container-based development environments
for Gitea repositories on Kubernetes. Create, stop, resume, and delete an
environment from Gitea, then work in a browser IDE or connect with SSH or SFTP.

## Getting started

After a Gitea administrator enables Codespaces:

1. Open a repository, commit, or pull request in Gitea.
2. Select **Create Codespace** and choose an environment and Dev Container
   configuration.
3. Follow the build log until the environment is ready.
4. Open the workspace in the browser, VS Code, or Zed, or use the displayed SSH
   and SFTP connection details.

The workspace persists while a Codespace is stopped, so it can be resumed
without cloning the repository again. The repository's Dev Container
configuration defines the project tools, while the selected environment defines
the Kubernetes isolation, storage, and resource policy.

## Application ports

Applications can publish HTTP ports from inside the Dev Container with the
`gitea-codespace-endpoint` command. Private endpoints follow the current Gitea
user's access. Public access is enabled explicitly for an endpoint.

## Installation

Cluster operators install Gitea Codespace with the OCI Helm chart. The
[Helm guide](charts/gitea-codespace/README.md) covers cluster requirements,
installation, runtime isolation, networking, and Gitea registration.

## Documentation

- [Helm installation and operations](charts/gitea-codespace/README.md)
- [Development](DEVELOPMENT.md)
- [Architecture](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)

## License

[MIT](LICENSE)
