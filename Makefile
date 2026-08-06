GO ?= go
ETCD ?= etcd

.PHONY: test
test: test-scripts
	$(GO) test -skip 'Test.*E2E' ./...

.PHONY: test-scripts
test-scripts:
	bash -n internal/provisioner/builtin/bootstrap.sh
	bash -n internal/devcontainerruntime/builtin/configure-git.sh
	bash -n internal/devcontainerruntime/builtin/start-web-ide.sh
	sh -n devcontainer/docker/builtin/update-user.sh
	bash -n scripts/test-etcd.sh

.PHONY: test-smoke
test-smoke:
	$(GO) test -count=1 -run '^(TestInfrastructureStatePersistsConfigAndEncryptedSiteSecret|TestInfrastructureAdminSiteAPIHidesSecret|TestInfrastructureAdminAPIRequiresBearerToken|TestRunWithConfigGatewayRoleSkipsWorkerRPC)$$' ./internal/app

.PHONY: test-etcd-required
test-etcd-required:
	ETCD=$(ETCD) GO=$(GO) bash scripts/test-etcd.sh single

.PHONY: test-etcd-cluster-required
test-etcd-cluster-required:
	ETCD=$(ETCD) GO=$(GO) bash scripts/test-etcd.sh cluster

.PHONY: test-etcd
test-etcd:
	if command -v $(ETCD) >/dev/null 2>&1; then \
		ETCD=$(ETCD) GO=$(GO) bash scripts/test-etcd.sh single; \
		ETCD=$(ETCD) GO=$(GO) bash scripts/test-etcd.sh cluster; \
	else \
		echo "etcd tests skipped: etcd binary is unavailable"; \
	fi

.PHONY: test-devcontainer-e2e-required
test-devcontainer-e2e-required:
	DEVCONTAINER_E2E=1 $(GO) test -p 1 -count=1 -timeout 30m -run 'TestDockerE2E' ./devcontainer/docker

.PHONY: test-e2e
test-e2e:
	CODESPACE_E2E_INCUS=1 $(GO) test -p 1 -count=1 -timeout 30m -run 'Test.*E2E' ./...

.PHONY: test-e2e-auto
test-e2e-auto:
	if command -v incus >/dev/null 2>&1 && incus info >/dev/null 2>&1; then \
		CODESPACE_E2E_INCUS=1 $(GO) test -p 1 -count=1 -timeout 30m -run 'Test.*E2E' ./...; \
	else \
		echo "Incus E2E skipped: incus client is unavailable"; \
	fi

.PHONY: test-e2e-required
test-e2e-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 $(GO) test -p 1 -count=1 -timeout 30m -run 'Test.*E2E' ./...

.PHONY: test-e2e-container-required
test-e2e-container-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_INSTANCE_TYPE=container $(GO) test -p 1 -count=1 -timeout 30m -run 'Test.*E2E' ./...

.PHONY: test-e2e-vm-required
test-e2e-vm-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_INSTANCE_TYPE=virtual-machine $(GO) test -p 1 -count=1 -timeout 30m -run 'Test.*E2E' ./...

.PHONY: test-e2e-incus-matrix-required
test-e2e-incus-matrix-required:
	$(MAKE) test-e2e-container-required
	$(MAKE) test-e2e-manager-container-required
	$(MAKE) test-e2e-vm-required
	$(MAKE) test-e2e-manager-vm-required

.PHONY: test-e2e-runtime-required
test-e2e-runtime-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_RUNTIME_LIFECYCLE=1 $(GO) test -p 1 -count=1 -timeout 30m -run 'TestIncusE2ENativeDevContainerLifecycle' ./internal/provisioner

.PHONY: test-e2e-manager-container-required
test-e2e-manager-container-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_MANAGER_LIFECYCLE=1 CODESPACE_E2E_INCUS_INSTANCE_TYPE=container $(GO) test -p 1 -count=1 -timeout 30m -run 'TestAppE2EManagerProcessIncusCreateStopResumeLifecycle' ./internal/app

.PHONY: test-e2e-manager-vm-required
test-e2e-manager-vm-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_MANAGER_LIFECYCLE=1 CODESPACE_E2E_INCUS_INSTANCE_TYPE=virtual-machine $(GO) test -p 1 -count=1 -timeout 30m -run 'TestAppE2EManagerProcessIncusCreateStopResumeLifecycle' ./internal/app
