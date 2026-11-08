FROM docker:28.5.1-dind

RUN apk add --no-cache \
    bash \
    ca-certificates \
    e2fsprogs \
    git \
    git-lfs \
    openssh-client \
    shadow \
    util-linux

COPY --chmod=0555 bin/gitea-codespace /usr/local/bin/gitea-codespace
COPY --chmod=0555 bin/gt /usr/local/libexec/gitea-codespace/gt
COPY bin/web-ide /usr/local/libexec/gitea-codespace/web-ide
COPY --chmod=0444 bin/gitea-codespace.vsix /usr/local/share/gitea-codespace/web-ide/gitea-codespace.vsix

ARG GITEA_CLI_VERSION
ARG GITEA_CLI_SHA256
ARG CODE_SERVER_VERSION

LABEL dev.gitea.codespace.gitea-cli.version=$GITEA_CLI_VERSION \
      dev.gitea.codespace.gitea-cli.sha256=$GITEA_CLI_SHA256 \
      dev.gitea.codespace.web-ide.version=$CODE_SERVER_VERSION

ENTRYPOINT ["/usr/local/bin/gitea-codespace"]
CMD ["serve"]
