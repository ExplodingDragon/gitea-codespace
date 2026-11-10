GO ?= go
CONTAINER_TOOL ?= docker
GO_BIN ?= $(shell bin="$$($(GO) env GOBIN)"; if test -n "$$bin"; then printf "%s" "$$bin"; else printf "%s/bin" "$$($(GO) env GOPATH)"; fi)
SHFMT ?= $(GO_BIN)/shfmt
ARCH ?= $(shell $(GO) env GOARCH)
IMAGE ?= gitea-codespace-web-ide-dsh:local
PROVIDER_SOURCE ?= https://github.com/ExplodingDragon/gitea-codespace
PROVIDER_REVISION ?= development
EXPORT_SCRIPT := ../export.sh
SHELL_SCRIPTS := build.sh $(EXPORT_SCRIPT) runtime/prepare runtime/serve

.PHONY: adapter-check check version bundle image clean
adapter-check:
	@set -e; for script in $(SHELL_SCRIPTS); do sh -n "$$script"; done
	$(SHFMT) -d -ln posix -i 2 $(SHELL_SCRIPTS)
	node --check install-packed-dsh.mjs
	node --check plugin/integrate.mjs
	node --check runtime/prepare.mjs
	node --test adapter.test.mjs

check: adapter-check
	./build.sh linux $(ARCH) .tmp/$(ARCH) check

version:
	@set -e; . ./version.env; printf '%s\n' "$$DSH_DEFAULT_VERSION"

bundle: adapter-check
	./build.sh linux $(ARCH) .tmp/$(ARCH)
	cp $(EXPORT_SCRIPT) .tmp/export.sh

image: bundle
	@set -e; . ./version.env; \
	$(CONTAINER_TOOL) build \
		--build-arg TARGETARCH="$(ARCH)" \
		--build-arg PROVIDER_VERSION="$$DSH_DEFAULT_VERSION" \
		--build-arg PROVIDER_SOURCE="$(PROVIDER_SOURCE)" \
		--build-arg PROVIDER_REVISION="$(PROVIDER_REVISION)" \
		-t $(IMAGE) .

clean:
	$(RM) -r .tmp
