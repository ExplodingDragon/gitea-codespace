ARG BUSYBOX_IMAGE=busybox:1.37.0
FROM ${BUSYBOX_IMAGE}

ARG TARGETARCH
ARG PROVIDER_VERSION
ARG PROVIDER_SOURCE
ARG PROVIDER_REVISION
COPY --chmod=0555 export.sh /usr/local/bin/export-web-ide
COPY .tmp/${TARGETARCH}/ /provider/

LABEL org.opencontainers.image.title="Gitea Codespace DeepSeek Harness Web IDE provider" \
      org.opencontainers.image.description="DeepSeek Harness browser IDE provider for Gitea Codespace" \
      org.opencontainers.image.version=$PROVIDER_VERSION \
      org.opencontainers.image.source=$PROVIDER_SOURCE \
      org.opencontainers.image.revision=$PROVIDER_REVISION \
      org.opencontainers.image.licenses="MIT" \
      dev.gitea.codespace.web-ide.metadata='{"metadataVersion":1,"protocolVersion":1,"provider":{"id":"dsh","name":"DeepSeek Harness"},"optionsSchema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"plugins":{"type":"array","items":{"type":"string","minLength":1},"maxItems":128,"uniqueItems":true,"default":[]},"patches":{"type":"array","items":{"type":"object"},"maxItems":128,"default":[]}},"additionalProperties":false}}'

ENTRYPOINT ["/usr/local/bin/export-web-ide"]
