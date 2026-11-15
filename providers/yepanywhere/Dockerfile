FROM scratch

ARG TARGETARCH
ARG PROVIDER_VERSION
ARG PROVIDER_SOURCE
ARG PROVIDER_REVISION
COPY .tmp/${TARGETARCH}/ /provider/

LABEL org.opencontainers.image.title="Gitea Codespace Yep Anywhere Web IDE provider" \
      org.opencontainers.image.description="Yep Anywhere browser IDE provider for Gitea Codespace" \
      org.opencontainers.image.version=$PROVIDER_VERSION \
      org.opencontainers.image.source=$PROVIDER_SOURCE \
      org.opencontainers.image.revision=$PROVIDER_REVISION \
      org.opencontainers.image.licenses="MIT" \
      dev.gitea.codespace.web-ide.metadata='{"metadataVersion":1,"protocolVersion":1,"provider":{"id":"yepanywhere","name":"Yep Anywhere"},"optionsSchema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"providers":{"type":"array","items":{"type":"string","enum":["claude","claude-gateway","claude-ollama","codex","codex-oss","opencode","pi"]},"minItems":1,"maxItems":7,"uniqueItems":true},"settings":{"type":"object","default":{}}},"additionalProperties":false}}'
