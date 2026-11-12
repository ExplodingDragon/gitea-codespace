FROM docker:28.5.1-dind

RUN apk add --no-cache \
    bash \
    ca-certificates \
    e2fsprogs \
    git \
    git-lfs \
    linux-tools-usbip \
    openssh-client \
    shadow \
    util-linux

COPY --chmod=0555 bin/gitea-codespace /usr/local/bin/gitea-codespace
COPY --chmod=0555 bin/gt /usr/local/libexec/gitea-codespace/gt

ARG GITEA_CLI_VERSION
ARG GITEA_CLI_SHA256

LABEL dev.gitea.codespace.gitea-cli.version=$GITEA_CLI_VERSION \
      dev.gitea.codespace.gitea-cli.sha256=$GITEA_CLI_SHA256

ENTRYPOINT ["/usr/local/bin/gitea-codespace"]
CMD ["serve"]
