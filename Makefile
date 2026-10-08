GO ?= go
PNPM ?= pnpm
CONTAINER_TOOL ?= docker
CODESPACE_IMAGE ?= gitea-codespace:local
KUBERNETES_RUNTIME_ISOLATION ?= sysbox
KUBERNETES_RUNTIME_CLASS ?= sysbox-runc
KUBERNETES_STORAGE_CLASS ?= local-path
CONTROLLER_GEN_VERSION := v0.21.0

.PHONY: generate-kubernetes test-kubernetes test-kubernetes-runtime test-kubernetes-agent test-kubernetes-components test-kubernetes-ha test-kubernetes-lifecycle test-kubernetes-gitea
generate-kubernetes:
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) object crd paths=./internal/cluster/api/... output:crd:artifacts:config=deploy/crds

test-kubernetes:
	CODESPACE_TEST_KUBERNETES=1 $(GO) test -p 1 -count=1 -timeout 5m ./internal/cluster/...

test-kubernetes-runtime:
	test -f "$(CODESPACE_TEST_DOCKER_ARCHIVE)" || { echo "CODESPACE_TEST_DOCKER_ARCHIVE must point to the exported BusyBox Docker archive"; exit 1; }
	test -n "$(CODESPACE_IMAGE)" || { echo "CODESPACE_IMAGE must use the manually imported platform image"; exit 1; }
	CODESPACE_TEST_KUBERNETES_RUNTIME=1 CODESPACE_TEST_DOCKER_ARCHIVE="$(CODESPACE_TEST_DOCKER_ARCHIVE)" CODESPACE_TEST_PLATFORM_IMAGE="$(CODESPACE_IMAGE)" CODESPACE_TEST_RUNTIME_ISOLATION="$(KUBERNETES_RUNTIME_ISOLATION)" CODESPACE_TEST_RUNTIME_CLASS="$(KUBERNETES_RUNTIME_CLASS)" CODESPACE_TEST_STORAGE_CLASS="$(KUBERNETES_STORAGE_CLASS)" $(GO) test -p 1 -count=1 -timeout 5m -run '^TestKubernetesE2EDockerPersistence$$' ./internal/cluster

test-kubernetes-agent:
	$(GO) test -c -tags netgo,osusergo -o bin/agent.test ./internal/agent
	CODESPACE_TEST_KUBERNETES_AGENT=1 CODESPACE_TEST_PLATFORM_IMAGE="$(CODESPACE_IMAGE)" CODESPACE_TEST_RUNTIME_ISOLATION="$(KUBERNETES_RUNTIME_ISOLATION)" CODESPACE_TEST_RUNTIME_CLASS="$(KUBERNETES_RUNTIME_CLASS)" CODESPACE_TEST_STORAGE_CLASS="$(KUBERNETES_STORAGE_CLASS)" $(GO) test -p 1 -count=1 -timeout 5m -run '^TestKubernetesE2EAgentProcesses$$' ./internal/cluster

test-kubernetes-components:
	CODESPACE_TEST_KUBERNETES_COMPONENTS=1 $(GO) test -p 1 -count=1 -timeout 5m -run '^TestKubernetesE2EComponentWorkloads$$' ./internal/cluster

test-kubernetes-ha:
	CODESPACE_TEST_KUBERNETES_HA=1 $(GO) test -p 1 -count=1 -timeout 5m -run '^TestKubernetesE2EManagerFailover$$' ./internal/cluster

test-kubernetes-lifecycle:
	test -n "$(CODESPACE_TEST_GITEA_LISTEN)" || { echo "CODESPACE_TEST_GITEA_LISTEN must be reachable from Runtime Pods"; exit 1; }
	test -n "$(CODESPACE_TEST_NODE_ADDRESS)" || { echo "CODESPACE_TEST_NODE_ADDRESS must identify a Kubernetes node"; exit 1; }
	CODESPACE_TEST_KUBERNETES_LIFECYCLE=1 CODESPACE_TEST_RUNTIME_ISOLATION="$(KUBERNETES_RUNTIME_ISOLATION)" CODESPACE_TEST_RUNTIME_CLASS="$(KUBERNETES_RUNTIME_CLASS)" CODESPACE_TEST_STORAGE_CLASS="$(KUBERNETES_STORAGE_CLASS)" CODESPACE_TEST_GITEA_LISTEN="$(CODESPACE_TEST_GITEA_LISTEN)" CODESPACE_TEST_NODE_ADDRESS="$(CODESPACE_TEST_NODE_ADDRESS)" $(GO) test -p 1 -count=1 -timeout 115m -run '^TestKubernetesE2EProductLifecycle$$' ./internal/cluster

test-kubernetes-gitea:
	@test -n "$$CODESPACE_TEST_GITEA_URL" || { echo "CODESPACE_TEST_GITEA_URL is required"; exit 1; }
	@test -n "$$CODESPACE_TEST_GITEA_MANAGER_ID" || { echo "CODESPACE_TEST_GITEA_MANAGER_ID is required"; exit 1; }
	@test -n "$$CODESPACE_TEST_GITEA_MANAGER_SECRET" || { echo "CODESPACE_TEST_GITEA_MANAGER_SECRET is required"; exit 1; }
	CODESPACE_TEST_KUBERNETES_GITEA=1 $(GO) test -p 1 -count=1 -timeout 2m -run '^TestKubernetesE2ERealGiteaHandshake$$' ./internal/cluster

.PHONY: format lint build build-linux images frontend frontend-check admin-dev test-helm
format:
	$(GO) fmt ./...
	$(PNPM) --dir web run format

lint: test-scripts frontend-check test-helm
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

test-helm:
	helm lint charts/gitea-codespace --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 --set admin.tokenSecret.name=codespace-admin
	helm template codespace charts/gitea-codespace --namespace codespace-system --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 --set admin.tokenSecret.name=codespace-admin >/dev/null

admin-dev:
	$(PNPM) --dir web run dev

.PHONY: test
test: test-scripts
	$(GO) test -p 1 -skip 'Test.*E2E' ./...

.PHONY: test-scripts
test-scripts:
	bash -n internal/devcontainerruntime/builtin/configure-git.sh
	bash -n internal/devcontainerruntime/builtin/start-web-ide.sh
	sh -n devcontainer/docker/builtin/update-user.sh

.PHONY: test-smoke
test-smoke:
	$(GO) test -p 1 -count=1 ./internal/accessticket ./internal/agent ./internal/cache ./internal/cluster ./internal/gateway ./internal/transport

.PHONY: test-devcontainer-e2e-required
test-devcontainer-e2e-required:
	DEVCONTAINER_E2E=1 $(GO) test -p 1 -count=1 -timeout 30m -run 'TestDockerE2E' ./devcontainer/docker

.PHONY: test-cache-s3
test-cache-s3:
	test -n "$(CODESPACE_TEST_S3_BUCKET)" || { echo "CODESPACE_TEST_S3_BUCKET is required"; exit 1; }
	$(GO) test -p 1 -count=1 -timeout 5m -run '^TestCacheS3E2E$$' ./internal/cache

.PHONY: test-e2e
test-e2e: test-kubernetes test-kubernetes-agent test-kubernetes-components test-kubernetes-ha test-kubernetes-gitea test-kubernetes-lifecycle
