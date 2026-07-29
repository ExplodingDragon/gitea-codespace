GO ?= go

.PHONY: test
test: test-scripts
	$(GO) test -skip 'Test.*E2E' ./...

.PHONY: test-scripts
test-scripts:
	bash -n internal/provisioner/builtin/bootstrap.sh

.PHONY: test-e2e
test-e2e:
	CODESPACE_E2E_INCUS=1 $(GO) test -count=1 -run 'Test.*E2E' ./...

.PHONY: test-e2e-auto
test-e2e-auto:
	if command -v incus >/dev/null 2>&1 && incus info >/dev/null 2>&1; then \
		CODESPACE_E2E_INCUS=1 $(GO) test -count=1 -run 'Test.*E2E' ./...; \
	else \
		echo "Incus E2E skipped: incus client is unavailable"; \
	fi

.PHONY: test-e2e-required
test-e2e-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 $(GO) test -count=1 -run 'Test.*E2E' ./...

.PHONY: test-e2e-container-required
test-e2e-container-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_INSTANCE_TYPE=container $(GO) test -count=1 -run 'Test.*E2E' ./...

.PHONY: test-e2e-vm-required
test-e2e-vm-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_INSTANCE_TYPE=virtual-machine $(GO) test -count=1 -run 'Test.*E2E' ./...

.PHONY: test-e2e-incus-matrix-required
test-e2e-incus-matrix-required:
	$(MAKE) test-e2e-container-required
	$(MAKE) test-e2e-manager-container-required
	$(MAKE) test-e2e-vm-required
	$(MAKE) test-e2e-manager-vm-required

.PHONY: test-e2e-runtime-required
test-e2e-runtime-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_RUNTIME_LIFECYCLE=1 $(GO) test -count=1 -run 'TestIncusE2ENativeDevContainerLifecycle' ./internal/provisioner

.PHONY: test-e2e-manager-container-required
test-e2e-manager-container-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_MANAGER_LIFECYCLE=1 CODESPACE_E2E_INCUS_INSTANCE_TYPE=container $(GO) test -count=1 -run 'TestAppE2EManagerProcessIncusCreateStopResumeLifecycle' ./internal/app

.PHONY: test-e2e-manager-vm-required
test-e2e-manager-vm-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_MANAGER_LIFECYCLE=1 CODESPACE_E2E_INCUS_INSTANCE_TYPE=virtual-machine $(GO) test -count=1 -run 'TestAppE2EManagerProcessIncusCreateStopResumeLifecycle' ./internal/app
