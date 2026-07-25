GO ?= go

.PHONY: test
test: test-scripts
	$(GO) test -skip 'Test.*E2E' ./...

.PHONY: test-scripts
test-scripts:
	sh -n examples/devcontainer/init.sh
	sh -n examples/devcontainer/start.sh
	sh -n examples/devcontainer/resume.sh

.PHONY: test-e2e
test-e2e:
	CODESPACE_E2E_INCUS=1 $(GO) test -count=1 -run 'Test.*E2E' ./...

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
test-e2e-incus-matrix-required: test-e2e-container-required test-e2e-vm-required

.PHONY: test-e2e-builtin-required
test-e2e-builtin-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_BUILTIN_LIFECYCLE=1 $(GO) test -count=1 -run 'TestIncusE2EBuiltinLifecycle' ./internal/provisioner

.PHONY: test-e2e-manager-required
test-e2e-manager-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_MANAGER_LIFECYCLE=1 $(GO) test -count=1 -run 'TestAppE2EManagerProcessIncusCreateStopResumeLifecycle' ./internal/app

.PHONY: test-e2e-manager-vm-required
test-e2e-manager-vm-required:
	CODESPACE_E2E_INCUS=1 CODESPACE_E2E_REQUIRE_INCUS=1 CODESPACE_E2E_INCUS_MANAGER_LIFECYCLE=1 CODESPACE_E2E_INCUS_INSTANCE_TYPE=virtual-machine $(GO) test -count=1 -run 'TestAppE2EManagerProcessIncusCreateStopResumeLifecycle' ./internal/app
