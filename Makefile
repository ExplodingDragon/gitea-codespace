GO ?= go
GOFMT ?= gofmt
PNPM ?= pnpm
CONTAINER_TOOL ?= docker
GO_BIN ?= $(shell bin="$$($(GO) env GOBIN)"; if test -n "$$bin"; then printf "%s" "$$bin"; else printf "%s/bin" "$$($(GO) env GOPATH)"; fi)
BUF ?= $(GO_BIN)/buf
SHFMT ?= $(GO_BIN)/shfmt
ACTIONLINT ?= $(GO_BIN)/actionlint
STATICCHECK ?= $(GO_BIN)/staticcheck
CODESPACE_IMAGE ?= gitea-codespace:local
WEB_IDE_E2E_IMAGE ?= localhost/gitea-codespace-web-ide-e2e:local
WEB_IDE_E2E_ARCH ?= amd64
CODESPACE_VERSION ?= development
KUBERNETES_RUNTIME_ISOLATION ?= sysbox
KUBERNETES_RUNTIME_CLASS ?= sysbox-runc
KUBERNETES_STORAGE_CLASS ?= local-path
CONTROLLER_GEN_VERSION := v0.21.0
BUF_VERSION := v1.8.0
PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_CONNECT_GO_VERSION := v2.0.1
SHFMT_VERSION := v3.14.1
ACTIONLINT_VERSION := v1.7.12
STATICCHECK_VERSION := v0.8.2
BASH_SCRIPTS := \
	tests/e2e/run.sh \
	tests/e2e/setup-k3s.sh
POSIX_SCRIPTS := \
	deploy/prepare-gitea-cli.sh \
	tests/e2e/web-ide/export.sh \
	tests/e2e/web-ide/prepare \
	tests/e2e/web-ide/serve \
	devcontainer/docker/builtin/resolve-user-shell.sh \
	devcontainer/docker/builtin/update-user.sh \
	internal/devcontainerruntime/builtin/configure-git.sh \
	internal/devcontainerruntime/builtin/endpoint.sh \
	internal/devcontainerruntime/builtin/install-ca.sh
GO_FILES := $(shell find . -path './.git' -prune -o -path './.tmp' -prune -o -path './web/node_modules' -prune -o -type f -name '*.go' -print)

define prepare-proto-workspace
mkdir -p .tmp; \
work="$$(mktemp -d .tmp/proto-work.XXXXXX)"; \
trap 'rm -rf "$$work"; rmdir .tmp 2>/dev/null || true' EXIT; \
mkdir -p "$$work/agent/v1" "$$work/component/v1" "$$work/codespace/v1"; \
cp internal/rpc/agent/v1/service.proto "$$work/agent/v1/service.proto"; \
cp internal/rpc/component/v1/service.proto "$$work/component/v1/service.proto"; \
proto_module="$$($(GO) list -m -f '{{.Dir}}' gitea.dev/codespace-proto-go)"; \
cp "$$proto_module/proto/codespace/v1/types.proto" "$$work/codespace/v1/types.proto";
endef

.PHONY: generate-kubernetes kubernetes-check
generate-kubernetes:
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) object crd paths=./internal/cluster/api/... output:crd:artifacts:config=deploy/crds
	cp deploy/crds/*.yaml charts/gitea-codespace/crds/

kubernetes-check:
	@mkdir -p .tmp; \
	work="$$(mktemp -d .tmp/kubernetes-work.XXXXXX)"; \
	trap 'rm -rf "$$work"; rmdir .tmp 2>/dev/null || true' EXIT; \
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) object crd paths=./internal/cluster/api/... output:crd:artifacts:config="$$work"; \
	diff -ru "$$work" deploy/crds; \
	diff -ru "$$work" charts/gitea-codespace/crds

.PHONY: install-tools proto-format proto-generate proto-check format lint go-format-check build build-linux prepare-gitea-cli images e2e-web-ide-image clean frontend frontend-check test test-frontend test-e2e admin-dev helm-check scripts-check scripts-format-check workflow-check
install-tools:
	$(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(GO) install connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@$(PROTOC_GEN_CONNECT_GO_VERSION)
	$(GO) install mvdan.cc/sh/v3/cmd/shfmt@$(SHFMT_VERSION)
	$(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	$(GO) install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)

proto-format:
	$(BUF) format internal/rpc/agent/v1/service.proto -w
	$(BUF) format internal/rpc/component/v1/service.proto -w

proto-generate:
	@$(prepare-proto-workspace) \
	PATH="$(GO_BIN):$$PATH" $(BUF) generate "$$work" --template buf.gen.yaml \
		--path "$$work/agent/v1/service.proto" --path "$$work/component/v1/service.proto"

proto-check:
	@$(prepare-proto-workspace) \
	$(BUF) lint "$$work" --path "$$work/agent/v1/service.proto" --path "$$work/component/v1/service.proto"; \
	$(BUF) format "$$work" --diff --exit-code --path "$$work/agent/v1/service.proto" --path "$$work/component/v1/service.proto"; \
	PATH="$(GO_BIN):$$PATH" $(BUF) generate "$$work" --template buf.gen.yaml --output "$$work/generated" \
		--path "$$work/agent/v1/service.proto" --path "$$work/component/v1/service.proto"; \
	diff -u internal/rpc/agent/v1/service.pb.go "$$work/generated/internal/rpc/agent/v1/service.pb.go"; \
	diff -u internal/rpc/agent/v1/agentv1connect/service.connect.go "$$work/generated/internal/rpc/agent/v1/agentv1connect/service.connect.go"; \
	diff -u internal/rpc/component/v1/service.pb.go "$$work/generated/internal/rpc/component/v1/service.pb.go"; \
	diff -u internal/rpc/component/v1/componentv1connect/service.connect.go "$$work/generated/internal/rpc/component/v1/componentv1connect/service.connect.go"

format:
	$(GO) fmt ./...
	$(SHFMT) -w -ln bash $(BASH_SCRIPTS)
	$(SHFMT) -w -ln posix -i 2 $(POSIX_SCRIPTS)
	$(MAKE) proto-format
	$(PNPM) --dir web run format

lint: go-format-check proto-check scripts-check scripts-format-check workflow-check frontend-check helm-check kubernetes-check
	$(GO) vet ./...
	$(STATICCHECK) ./...

go-format-check:
	@unformatted="$$($(GOFMT) -l $(GO_FILES))"; test -z "$$unformatted" || { printf 'Go files need formatting:\n%s\n' "$$unformatted" >&2; exit 1; }

build: frontend
	$(GO) build -ldflags="-X 'gitea.dev/codespace/internal/controlplane.buildVersion=$(CODESPACE_VERSION)'" -o bin/gitea-codespace .

build-linux: frontend
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w -X 'gitea.dev/codespace/internal/controlplane.buildVersion=$(CODESPACE_VERSION)'" -o bin/gitea-codespace .

prepare-gitea-cli:
	GITEA_CLI_VERSION="$(GITEA_CLI_VERSION)" ./deploy/prepare-gitea-cli.sh linux amd64 bin

images: build-linux prepare-gitea-cli
	@. bin/gitea-cli.env; \
	$(CONTAINER_TOOL) build \
		--build-arg GITEA_CLI_VERSION="$$GITEA_CLI_VERSION" \
		--build-arg GITEA_CLI_SHA256="$$GITEA_CLI_SHA256" \
		-f deploy/Dockerfile -t $(CODESPACE_IMAGE) .

e2e-web-ide-image:
	$(RM) -r .tmp/e2e-web-ide
	mkdir -p .tmp/e2e-web-ide/layout
	CGO_ENABLED=0 GOOS=linux GOARCH=$(WEB_IDE_E2E_ARCH) $(GO) build -trimpath -ldflags='-s -w' -o .tmp/e2e-web-ide/web-ide-e2e ./tests/e2e/web-ide
	@if $(CONTAINER_TOOL) --version | grep -qi podman; then \
		$(CONTAINER_TOOL) build --platform linux/$(WEB_IDE_E2E_ARCH) --build-arg TARGETARCH=$(WEB_IDE_E2E_ARCH) --tag $(WEB_IDE_E2E_IMAGE) -f tests/e2e/web-ide/Dockerfile .; \
		$(CONTAINER_TOOL) save --format oci-archive --output .tmp/e2e-web-ide/image.tar $(WEB_IDE_E2E_IMAGE); \
	else \
		$(CONTAINER_TOOL) buildx build --platform linux/$(WEB_IDE_E2E_ARCH) --tag $(WEB_IDE_E2E_IMAGE) --output type=oci,dest=.tmp/e2e-web-ide/image.tar -f tests/e2e/web-ide/Dockerfile .; \
	fi
	tar -xf .tmp/e2e-web-ide/image.tar -C .tmp/e2e-web-ide/layout
	printf '%s\n' '$(WEB_IDE_E2E_IMAGE)' >.tmp/e2e-web-ide/tag

clean:
	$(RM) -r bin .tmp web/dist web/test-results web/playwright-report
	mkdir -p web/dist
	touch web/dist/.gitkeep

frontend:
	$(PNPM) --dir web install --frozen-lockfile
	$(PNPM) --dir web run build

frontend-check:
	$(PNPM) --dir web run check
	$(PNPM) --dir web run format:check

test-frontend:
	$(PNPM) --dir web run test

helm-check:
	helm lint charts/gitea-codespace --set admin.tokenSecret.name=codespace-admin
	@rendered="$$(helm template codespace charts/gitea-codespace --namespace codespace-system --set admin.tokenSecret.name=codespace-admin)"; \
	printf '%s\n' "$$rendered" | grep -Fq 'image: "ghcr.io/explodingdragon/gitea-codespace:main"'; \
	printf '%s\n' "$$rendered" | grep -Fq 'imagePullPolicy: Always'; \
	printf '%s\n' "$$rendered" | grep -Fq -- '--web-ide-program-volume-size=4Gi'; \
	printf '%s\n' "$$rendered" | grep -Fq -- '--identity-secret=codespace-gitea-codespace-internal-ca'
	helm lint charts/gitea-codespace --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 --set admin.tokenSecret.name=codespace-admin
	@rendered="$$(helm template codespace charts/gitea-codespace --namespace codespace-system --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 --set admin.tokenSecret.name=codespace-admin)"; \
	printf '%s\n' "$$rendered" | grep -Fq 'image: "ghcr.io/explodingdragon/gitea-codespace@sha256:0000000000000000000000000000000000000000000000000000000000000000"'; \
	printf '%s\n' "$$rendered" | grep -Fq 'imagePullPolicy: IfNotPresent'

admin-dev:
	$(PNPM) --dir web run dev

test:
	$(GO) test -p 1 -skip 'Test.*E2E' ./...

scripts-check:
	@set -e; for script in $(BASH_SCRIPTS); do bash -n "$$script"; done
	@set -e; for script in $(POSIX_SCRIPTS); do sh -n "$$script"; done

scripts-format-check:
	$(SHFMT) -d -ln bash $(BASH_SCRIPTS)
	$(SHFMT) -d -ln posix -i 2 $(POSIX_SCRIPTS)

workflow-check:
	$(ACTIONLINT) .github/workflows/*.yml

test-e2e:
	GO="$(GO)" \
	CODESPACE_E2E_PLATFORM_IMAGE="$(CODESPACE_IMAGE)" \
	CODESPACE_E2E_WEB_IDE_IMAGE="$(WEB_IDE_E2E_IMAGE)" \
	CODESPACE_E2E_WEB_IDE_LAYOUT="$(CURDIR)/.tmp/e2e-web-ide/layout" \
	CODESPACE_E2E_WEB_IDE_ARCHIVE="$(CURDIR)/.tmp/e2e-web-ide/image.tar" \
	CODESPACE_E2E_RUNTIME_ISOLATION="$(KUBERNETES_RUNTIME_ISOLATION)" \
	CODESPACE_E2E_RUNTIME_CLASS="$(KUBERNETES_RUNTIME_CLASS)" \
	CODESPACE_E2E_STORAGE_CLASS="$(KUBERNETES_STORAGE_CLASS)" \
	tests/e2e/run.sh
