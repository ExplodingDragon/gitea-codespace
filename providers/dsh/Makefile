GO ?= go
CONTAINER_TOOL ?= docker
GO_BIN ?= $(shell bin="$$($(GO) env GOBIN)"; if test -n "$$bin"; then printf "%s" "$$bin"; else printf "%s/bin" "$$($(GO) env GOPATH)"; fi)
SHFMT ?= $(GO_BIN)/shfmt
ARCH ?= $(shell $(GO) env GOARCH)
IMAGE ?= gitea-codespace-web-ide-dsh:local
PROVIDER_SOURCE ?= https://github.com/ExplodingDragon/gitea-codespace
PROVIDER_REVISION ?= development

.PHONY: adapter-check check version bundle image clean
adapter-check:
	$(SHFMT) -d -ln posix -i 2 build.sh export.sh runtime/prepare runtime/serve
	node --check install-packed-dsh.mjs
	node --check plugin/integrate.mjs
	node --check runtime/prepare.mjs
	node --test adapter.test.mjs

check: adapter-check
	./build.sh linux $(ARCH) .tmp/$(ARCH) check

version:
	@. ./version.env; printf '%s\n' "$$DSH_DEFAULT_VERSION"

bundle: adapter-check
	./build.sh linux $(ARCH) .tmp/$(ARCH)

image: bundle
	@. ./version.env; \
	$(CONTAINER_TOOL) build \
		--build-arg TARGETARCH="$(ARCH)" \
		--build-arg PROVIDER_VERSION="$$DSH_DEFAULT_VERSION" \
		--build-arg PROVIDER_SOURCE="$(PROVIDER_SOURCE)" \
		--build-arg PROVIDER_REVISION="$(PROVIDER_REVISION)" \
		-t $(IMAGE) .

clean:
	$(RM) -r .tmp
