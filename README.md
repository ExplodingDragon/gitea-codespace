# Gitea Codespace

Gitea Codespace provides isolated, Dev Container-based development
environments for Gitea repositories on Kubernetes. Users create and manage
their environments from Gitea, then connect through a browser, SSH, or SFTP.

## Use a Codespace

After an administrator connects Gitea to a Codespace deployment:

1. open a supported repository, commit, or pull request page in Gitea;
2. choose **Create Codespace**, then select an available runtime environment
   and Dev Container configuration;
3. follow creation progress and logs from the Codespace page; and
4. open the environment in the browser or connect through SSH or SFTP.

Codespaces retain their workspace when stopped and can be resumed later.
Repository Dev Container configuration controls the development environment,
while administrators provide reusable runtime templates and optional shared
image caching.

## Access and ports

The Codespace page lists the available browser, SSH, and SFTP connections.
Applications can publish HTTP ports from inside the Dev Container with the
`gitea-codespace-endpoint` command. Private endpoints follow the current Gitea
user's access, while public endpoints are explicitly selected when published.

## Deploy

Cluster operators install Gitea Codespace with its OCI Helm chart. A deployment
requires Kubernetes 1.31 or later, Helm 4.3 or later, a Gitea installation with
Codespace support, and a verified Kata Containers or Sysbox runtime. Follow the
[Helm installation and operations guide](charts/gitea-codespace/README.md) to
prepare isolation, storage, network entry points, and the Gitea registration.

## Documentation

- [Helm installation and operations](charts/gitea-codespace/README.md) for
  cluster administrators
- [Developer guide](DEVELOPMENT.md)
- [Architecture and design](https://github.com/ExplodingDragon/gitea-dev/tree/main/src/codespace)

## License

[MIT](LICENSE)
