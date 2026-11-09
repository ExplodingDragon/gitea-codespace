GO ?= go
PNPM ?= pnpm
CONTAINER_TOOL ?= docker
GO_BIN ?= $(shell bin="$$($(GO) env GOBIN)"; if test -n "$$bin"; then printf "%s" "$$bin"; else printf "%s/bin" "$$($(GO) env GOPATH)"; fi)
SHFMT ?= $(GO_BIN)/shfmt
ARCH ?= $(shell $(GO) env GOARCH)
IMAGE ?= gitea-codespace-web-ide-vscode:local
PROVIDER_SOURCE ?= https://github.com/ExplodingDragon/gitea-codespace
PROVIDER_REVISION ?= development

.PHONY: check version bundle image clean
check:
	$(SHFMT) -d -ln posix -i 2 build.sh export.sh runtime/prepare runtime/serve
	node --check runtime/prepare.mjs
	node --test adapter.test.mjs
	$(PNPM) --dir extension install --frozen-lockfile
	$(PNPM) --dir extension run check
	$(PNPM) --dir extension run format:check
	$(PNPM) --dir extension run test

version:
	@. ./version.env; printf '%s\n' "$$CODE_SERVER_DEFAULT_VERSION"

bundle: check
	$(PNPM) --dir extension run build
	./build.sh linux $(ARCH) .tmp/$(ARCH)

image: bundle
	@. ./version.env; \
	$(CONTAINER_TOOL) build \
		--build-arg TARGETARCH="$(ARCH)" \
		--build-arg PROVIDER_VERSION="$$CODE_SERVER_DEFAULT_VERSION" \
		--build-arg PROVIDER_SOURCE="$(PROVIDER_SOURCE)" \
		--build-arg PROVIDER_REVISION="$(PROVIDER_REVISION)" \
		-t $(IMAGE) .

clean:
	$(RM) -r .tmp extension/dist
