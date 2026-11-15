GO ?= go
CONTAINER_TOOL ?= docker
GO_BIN ?= $(shell bin="$$($(GO) env GOBIN)"; if test -n "$$bin"; then printf "%s" "$$bin"; else printf "%s/bin" "$$($(GO) env GOPATH)"; fi)
SHFMT ?= $(GO_BIN)/shfmt
ARCH ?= $(shell $(GO) env GOARCH)
IMAGE ?= gitea-codespace-web-ide-yepanywhere:local
PROVIDER_SOURCE ?= https://github.com/ExplodingDragon/gitea-codespace
PROVIDER_REVISION ?= development
SHELL_SCRIPTS := build.sh runtime/prepare runtime/serve tools/launch

.PHONY: adapter-check check version bundle image clean
adapter-check:
	@set -e; for script in $(SHELL_SCRIPTS); do sh -n "$$script"; done
	$(SHFMT) -d -ln posix -i 2 $(SHELL_SCRIPTS)
	@set -e; for script in integrate.mjs runtime/*.mjs tools/*.mjs; do node --check "$$script"; done
	node --test adapter.test.mjs

check: adapter-check
	./build.sh linux $(ARCH) .tmp/$(ARCH) check

version:
	@set -e; . ./version.env; printf '%s\n' "$$YEP_VERSION"

bundle: adapter-check
	./build.sh linux $(ARCH) .tmp/$(ARCH)

image: bundle
	@set -e; version="$$(node -p "require('./.tmp/$(ARCH)/manifest.json').version")"; \
	$(CONTAINER_TOOL) build \
		--build-arg TARGETARCH="$(ARCH)" \
		--build-arg PROVIDER_VERSION="$$version" \
		--build-arg PROVIDER_SOURCE="$(PROVIDER_SOURCE)" \
		--build-arg PROVIDER_REVISION="$(PROVIDER_REVISION)" \
		-t $(IMAGE) .

clean:
	$(RM) -r .tmp
