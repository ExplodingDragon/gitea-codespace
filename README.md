# Gitea Codespace

Gitea Codespace creates remote development environments from a repository's
Dev Container configuration. A workspace can be used from a browser, a desktop
editor, or through SSH and SFTP.

## Start a codespace

After an administrator connects a Gitea instance:

1. Open a repository, commit, or pull request.
2. Select **Create Codespace**.
3. Choose a Dev Container configuration and environment.
4. Open the workspace after the build completes.

The codespace page provides the available browser editor, SSH connection, and
published application links. Stopping a codespace preserves the workspace for
its next start.

The project publishes VS Code and DeepSeek Harness browser images. An
administrator can select either image or another compatible Web IDE provider
for each environment.

## Documentation

- [Install and operate with Helm](charts/gitea-codespace/README.md)
- [Develop and test Codespace](DEVELOPMENT.md)
- [Build a Web IDE provider](providers/README.md)

## License

[MIT](LICENSE)
