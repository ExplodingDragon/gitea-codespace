GO ?= go
PNPM ?= pnpm
CONTAINER_TOOL ?= docker
GO_BIN ?= $(shell bin="$$($(GO) env GOBIN)"; if test -n "$$bin"; then printf "%s" "$$bin"; else printf "%s/bin" "$$($(GO) env GOPATH)"; fi)
BUF ?= $(GO_BIN)/buf
CODESPACE_IMAGE ?= gitea-codespace:local
KUBERNETES_RUNTIME_ISOLATION ?= sysbox
KUBERNETES_RUNTIME_CLASS ?= sysbox-runc
KUBERNETES_STORAGE_CLASS ?= local-path
CONTROLLER_GEN_VERSION := v0.21.0
BUF_VERSION := v1.8.0
PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_CONNECT_GO_VERSION := v1.20.0

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

.PHONY: generate-kubernetes
generate-kubernetes:
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) object crd paths=./internal/cluster/api/... output:crd:artifacts:config=deploy/crds

.PHONY: install-proto proto-format proto-generate proto-check format lint build build-linux images frontend frontend-check admin-dev helm-check scripts-check
install-proto:
	$(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(GO) install connectrpc.com/connect/cmd/protoc-gen-connect-go@$(PROTOC_GEN_CONNECT_GO_VERSION)

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
	$(MAKE) proto-format
	$(PNPM) --dir web run format

lint: proto-check scripts-check frontend-check helm-check
	$(GO) vet ./...

build: frontend
	$(GO) build -o bin/gitea-codespace .

build-linux: frontend
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -o bin/gitea-codespace .

images: build-linux
	$(CONTAINER_TOOL) build -f deploy/Dockerfile -t $(CODESPACE_IMAGE) .

frontend:
	$(PNPM) --dir web install --frozen-lockfile
	$(PNPM) --dir web run build

frontend-check:
	$(PNPM) --dir web run check
	$(PNPM) --dir web run format:check

helm-check:
	helm lint charts/gitea-codespace --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 --set admin.tokenSecret.name=codespace-admin
	helm template codespace charts/gitea-codespace --namespace codespace-system --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 --set admin.tokenSecret.name=codespace-admin >/dev/null

admin-dev:
	$(PNPM) --dir web run dev

.PHONY: test
test: scripts-check
	$(GO) test -p 1 -skip 'Test.*E2E' ./...

scripts-check:
	bash -n internal/devcontainerruntime/builtin/configure-git.sh
	bash -n internal/devcontainerruntime/builtin/start-web-ide.sh
	bash -n tests/e2e/run.sh
	sh -n devcontainer/docker/builtin/update-user.sh

.PHONY: test-smoke
test-smoke:
	$(GO) test -p 1 -count=1 ./internal/accessticket ./internal/agent ./internal/cache ./internal/cluster ./internal/gateway ./internal/transport

.PHONY: test-e2e
test-e2e:
	GO="$(GO)" \
	CODESPACE_E2E_PLATFORM_IMAGE="$(CODESPACE_IMAGE)" \
	CODESPACE_E2E_RUNTIME_ISOLATION="$(KUBERNETES_RUNTIME_ISOLATION)" \
	CODESPACE_E2E_RUNTIME_CLASS="$(KUBERNETES_RUNTIME_CLASS)" \
	CODESPACE_E2E_STORAGE_CLASS="$(KUBERNETES_STORAGE_CLASS)" \
	tests/e2e/run.sh
