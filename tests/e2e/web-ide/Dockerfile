FROM busybox:1.37.0

ARG TARGETARCH
COPY --chmod=0555 .tmp/e2e-web-ide/web-ide-e2e /provider/bin/web-ide-e2e
COPY --chmod=0555 tests/e2e/web-ide/prepare /provider/bin/prepare
COPY --chmod=0555 tests/e2e/web-ide/serve /provider/bin/serve
COPY --chmod=0555 tests/e2e/web-ide/export.sh /usr/local/bin/export-web-ide

RUN printf '{"schemaVersion":1,"provider":"e2e","architecture":"%s","version":"1"}\n' "$TARGETARCH" >/provider/manifest.json

LABEL org.opencontainers.image.title="Gitea Codespace E2E Web IDE" \
      org.opencontainers.image.version="1" \
      org.opencontainers.image.source="https://github.com/ExplodingDragon/gitea-codespace" \
      org.opencontainers.image.revision="e2e" \
      dev.gitea.codespace.web-ide.metadata='{"metadataVersion":1,"protocolVersion":1,"provider":{"id":"e2e","name":"E2E Web IDE"},"optionsSchema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"text":{"type":"string","default":"template"},"enabled":{"type":"boolean","default":true},"count":{"type":"integer","minimum":0,"default":2},"ratio":{"type":"number","minimum":0,"default":1.5},"mode":{"type":"string","enum":["fast","safe"],"default":"safe"},"items":{"type":"array","items":{"type":"string"},"default":["one","two"]},"nested":{"type":"object","properties":{"value":{"type":"string","default":"nested"}},"additionalProperties":false}},"additionalProperties":false}}'

USER 0:0
ENTRYPOINT ["/usr/local/bin/export-web-ide"]
