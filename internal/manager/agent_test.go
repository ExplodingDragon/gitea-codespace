// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/provisioner"
)

func TestAgentHandlesCreateOperation(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	stateStore := &memoryOperationStateStore{}
	credentialStore := &memoryRuntimeCredentialStore{}
	metadataStore := &memoryRuntimeMetadataStateStore{}
	service := &managerService{
		finalized: make(chan struct{}, 1),
		operation: &codespacev1.OperationPayload{
			OperationRversion:         1,
			CodespaceUuid:             codespaceUUID,
			LogOffset:                 0,
			LeaseValidForMilliseconds: 30000,
			Command: &codespacev1.OperationPayload_Create{
				Create: &codespacev1.CreateOperationPayload{
					RepoFullName:     "owner/repo",
					RepoCloneHttpUrl: "https://gitea.example.com/owner/repo.git",
					RepoCloneSshUrl:  "git@gitea.example.com:owner/repo.git",
					RepoTag:          "default",
					GitProtocol:      codespacev1.GitProtocol_GIT_PROTOCOL_HTTP,
					RuntimeSettings:  &codespacev1.EffectiveCodespaceRuntimeSettings{},
					CommitSha:        "0123456789abcdef0123456789abcdef01234567",
				},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	provisioner := newCredentialTrackingProvisioner()
	agent := New(AgentConfig{
		BaseURL:                   server.URL,
		ManagerID:                 7,
		ManagerSecret:             "manager-secret",
		Name:                      "test-manager",
		GatewayURL:                "https://workspace.example.net",
		GatewaySSHAddr:            "workspace.example.net:22",
		Version:                   "test",
		Tags:                      []string{"default"},
		CapacityTotal:             1,
		CapacityAvailable:         1,
		CleanupCapacityAvailable:  1,
		MaxOperations:             1,
		RuntimeMetadataGeneration: 1,
		OperationStateStore:       stateStore,
		RuntimeCredentialStore:    credentialStore,
		RuntimeMetadataStateStore: metadataStore,
	}, server.Client(), provisioner)

	if err := agent.declare(context.Background(), codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := agent.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitFinalized(t, service.finalized)

	if !service.sawDeclare {
		t.Fatalf("declare was not called")
	}
	if !service.sawFetch {
		t.Fatalf("fetch was not called")
	}
	if !service.sawToken {
		t.Fatalf("request gitea token was not called")
	}
	if !service.sawMetadata {
		t.Fatalf("report metadata was not called")
	}
	if service.finalStatus != codespacev1.FinalStatus_FINAL_STATUS_DONE {
		t.Fatalf("final status = %s", service.finalStatus)
	}
	if service.finalOperationType != codespacev1.OperationType_OPERATION_TYPE_CREATE {
		t.Fatalf("final operation type = %s", service.finalOperationType)
	}
	if service.metadataGeneration != 6 {
		t.Fatalf("metadata generation = %d", service.metadataGeneration)
	}
	if got := service.runtimeMetadataStages(); !slices.Equal(got, expectedRuntimeBootStages()) {
		t.Fatalf("metadata stages = %#v", got)
	}
	saved := credentialStore.savedTokens()
	if len(saved) != 1 || saved[0].codespaceUUID != codespaceUUID || saved[0].token == "" {
		t.Fatalf("runtime credentials = %#v", saved)
	}
	if records := provisioner.credentialWrites(); len(records) != 1 ||
		records[0].instanceName == "" ||
		records[0].request.CodespaceUUID != codespaceUUID ||
		records[0].request.GiteaToken != "gcs_test" ||
		records[0].request.RuntimeToken != saved[0].token {
		t.Fatalf("credential writes = %#v saved=%#v", records, saved)
	}
	if snapshots := metadataStore.savedSnapshots(); len(snapshots) != 6 ||
		snapshots[0].CodespaceUUID != codespaceUUID ||
		snapshots[0].MetadataGeneration != 1 ||
		snapshots[0].Boot.Stage != RuntimeBootStagePrepareRuntime ||
		snapshots[5].MetadataGeneration != 6 ||
		snapshots[5].Boot.Stage != RuntimeBootStageReady ||
		snapshots[5].InternalSSH.Host == "" {
		t.Fatalf("runtime metadata snapshots = %#v", snapshots)
	}
	if service.managerID != "7" || service.managerSecret != "manager-secret" {
		t.Fatalf("manager auth headers = %q/%q", service.managerID, service.managerSecret)
	}
	if stateStore.savedCount() != 1 {
		t.Fatalf("saved active operations = %d", stateStore.savedCount())
	}
	if stateStore.savedStage() != OperationWorkerStageActive {
		t.Fatalf("saved worker stage = %q", stateStore.savedStage())
	}
	waitDeleted(t, stateStore, 1)
	if stateStore.deletedCount() != 1 {
		t.Fatalf("deleted active operations = %d", stateStore.deletedCount())
	}
}

func TestAgentReadyMetadataUsesPublisher(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	metadataStore := &memoryRuntimeMetadataStateStore{}
	publisher := &memoryRuntimeMetadataPublisher{}
	agent := New(AgentConfig{
		BaseURL:                   "http://127.0.0.1",
		RuntimeMetadataGeneration: 1,
		RuntimeMetadataStateStore: metadataStore,
		RuntimeMetadataPublisher:  publisher,
	}, http.DefaultClient, nil)
	err := agent.reportReadyMetadata(
		context.Background(),
		&codespacev1.OperationPayload{
			CodespaceUuid:     codespaceUUID,
			OperationRversion: 7,
		},
		&provisioner.Instance{
			InternalSSHHost:        "10.0.0.12",
			InternalSSHPort:        2222,
			InternalSSHUser:        "dev",
			InternalSSHAuthMode:    "publickey",
			InternalSSHFingerprint: "SHA256:runtime",
		},
	)
	if err != nil {
		t.Fatalf("report ready metadata: %v", err)
	}

	snapshots := metadataStore.savedSnapshots()
	if len(snapshots) != 1 || snapshots[0].CodespaceUUID != codespaceUUID || snapshots[0].Boot.OperationRVersion != 7 {
		t.Fatalf("saved snapshots = %#v", snapshots)
	}
	if calls := publisher.calls(); len(calls) != 1 || calls[0] != codespaceUUID {
		t.Fatalf("publisher calls = %#v", calls)
	}
}

func TestAgentStopAndDeleteCloseCodespaceAccess(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	access := &memoryAccessController{}
	agent := New(AgentConfig{
		BaseURL:          "http://127.0.0.1",
		AccessController: access,
	}, http.DefaultClient, provisioner.NewDummy())

	stopOperation := &codespacev1.OperationPayload{CodespaceUuid: codespaceUUID}
	if err := agent.handleStop(context.Background(), stopOperation); err != nil {
		t.Fatalf("handle stop: %v", err)
	}
	deleteOperation := &codespacev1.OperationPayload{CodespaceUuid: codespaceUUID}
	if err := agent.handleDelete(context.Background(), deleteOperation, false); err != nil {
		t.Fatalf("handle delete: %v", err)
	}

	if calls := access.calls(); len(calls) != 2 || calls[0] != codespaceUUID || calls[1] != codespaceUUID {
		t.Fatalf("access close calls = %#v", calls)
	}
}

func TestAgentHandlesResumeOperationWritesCredentials(t *testing.T) {
	t.Parallel()

	codespaceUUID := "12121212-1212-4212-8212-121212121212"
	credentialStore := &memoryRuntimeCredentialStore{}
	service := &managerService{
		finalized:                 make(chan struct{}, 1),
		metadataOperationRVersion: 2,
		operation: &codespacev1.OperationPayload{
			OperationRversion:         2,
			CodespaceUuid:             codespaceUUID,
			LogOffset:                 0,
			LeaseValidForMilliseconds: 30000,
			Command: &codespacev1.OperationPayload_Resume{
				Resume: &codespacev1.ResumeOperationPayload{
					RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{},
				},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	trackedProvisioner := newCredentialTrackingProvisioner()
	if _, err := trackedProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create existing instance: %v", err)
	}
	agent := New(AgentConfig{
		BaseURL:                   server.URL,
		ManagerID:                 7,
		ManagerSecret:             "manager-secret",
		Name:                      "test-manager",
		GatewayURL:                "https://workspace.example.net",
		GatewaySSHAddr:            "workspace.example.net:22",
		Version:                   "test",
		Tags:                      []string{"default"},
		CapacityTotal:             1,
		CapacityAvailable:         1,
		CleanupCapacityAvailable:  1,
		MaxOperations:             1,
		RuntimeMetadataGeneration: 1,
		RuntimeCredentialStore:    credentialStore,
	}, server.Client(), trackedProvisioner)

	if err := agent.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitFinalized(t, service.finalized)
	if service.finalStatus != codespacev1.FinalStatus_FINAL_STATUS_DONE {
		t.Fatalf("final status = %s", service.finalStatus)
	}
	if service.finalOperationType != codespacev1.OperationType_OPERATION_TYPE_RESUME {
		t.Fatalf("final operation type = %s", service.finalOperationType)
	}
	if service.metadataGeneration != 6 {
		t.Fatalf("metadata generation = %d", service.metadataGeneration)
	}
	if got := service.runtimeMetadataStages(); !slices.Equal(got, expectedRuntimeBootStages()) {
		t.Fatalf("metadata stages = %#v", got)
	}
	saved := credentialStore.savedTokens()
	if len(saved) != 1 || saved[0].codespaceUUID != codespaceUUID || saved[0].token == "" {
		t.Fatalf("runtime credentials = %#v", saved)
	}
	if records := trackedProvisioner.credentialWrites(); len(records) != 1 ||
		records[0].request.CodespaceUUID != codespaceUUID ||
		records[0].request.GiteaToken != "gcs_test" ||
		records[0].request.RuntimeToken != saved[0].token {
		t.Fatalf("credential writes = %#v saved=%#v", records, saved)
	}
}

func TestAgentReportsObservedOperationWhileRunning(t *testing.T) {
	t.Parallel()

	codespaceUUID := "22222222-2222-4222-8222-222222222222"
	service := &managerService{
		finalized: make(chan struct{}, 1),
		operation: &codespacev1.OperationPayload{
			OperationRversion:         2,
			CodespaceUuid:             codespaceUUID,
			LogOffset:                 0,
			LeaseValidForMilliseconds: 30000,
			Command: &codespacev1.OperationPayload_Create{
				Create: &codespacev1.CreateOperationPayload{
					RepoFullName:     "owner/repo",
					RepoCloneHttpUrl: "https://gitea.example.com/owner/repo.git",
					RepoCloneSshUrl:  "git@gitea.example.com:owner/repo.git",
					RepoTag:          "default",
					GitProtocol:      codespacev1.GitProtocol_GIT_PROTOCOL_HTTP,
					RuntimeSettings:  &codespacev1.EffectiveCodespaceRuntimeSettings{},
					CommitSha:        "0123456789abcdef0123456789abcdef01234567",
				},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	provisioner := newBlockingProvisioner()
	agent := New(AgentConfig{
		BaseURL:                   server.URL,
		ManagerID:                 7,
		ManagerSecret:             "manager-secret",
		Name:                      "test-manager",
		GatewayURL:                "https://workspace.example.net",
		GatewaySSHAddr:            "workspace.example.net:22",
		Version:                   "test",
		Tags:                      []string{"default"},
		CapacityTotal:             1,
		CapacityAvailable:         1,
		CleanupCapacityAvailable:  1,
		MaxOperations:             1,
		RuntimeMetadataGeneration: 1,
	}, server.Client(), provisioner)

	if err := agent.pollOnce(context.Background()); err != nil {
		t.Fatalf("first poll once: %v", err)
	}
	waitStarted(t, provisioner.started)
	if err := agent.pollOnce(context.Background()); err != nil {
		t.Fatalf("second poll once: %v", err)
	}
	observed := service.observedOperations()
	if len(observed) != 1 {
		t.Fatalf("observed operations = %d", len(observed))
	}
	if observed[0].GetCodespaceUuid() != codespaceUUID || observed[0].GetOperationRversion() != 2 {
		t.Fatalf("observed operation = %s version %d", observed[0].GetCodespaceUuid(), observed[0].GetOperationRversion())
	}

	close(provisioner.release)
	waitFinalized(t, service.finalized)
}

func TestAgentResumesLoadedOperationAfterRenewal(t *testing.T) {
	t.Parallel()

	codespaceUUID := "33333333-3333-4333-8333-333333333333"
	operation := &codespacev1.OperationPayload{
		OperationRversion:         3,
		CodespaceUuid:             codespaceUUID,
		LogOffset:                 0,
		LeaseValidForMilliseconds: 30000,
		Command: &codespacev1.OperationPayload_Create{
			Create: &codespacev1.CreateOperationPayload{
				RepoFullName:     "owner/repo",
				RepoCloneHttpUrl: "https://gitea.example.com/owner/repo.git",
				RepoCloneSshUrl:  "git@gitea.example.com:owner/repo.git",
				RepoTag:          "default",
				GitProtocol:      codespacev1.GitProtocol_GIT_PROTOCOL_HTTP,
				RuntimeSettings:  &codespacev1.EffectiveCodespaceRuntimeSettings{},
				CommitSha:        "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
	stateStore := &memoryOperationStateStore{}
	service := &managerService{
		finalized:     make(chan struct{}, 1),
		renewObserved: true,
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:                   server.URL,
		ManagerID:                 7,
		ManagerSecret:             "manager-secret",
		Name:                      "test-manager",
		GatewayURL:                "https://workspace.example.net",
		GatewaySSHAddr:            "workspace.example.net:22",
		Version:                   "test",
		Tags:                      []string{"default"},
		CapacityTotal:             1,
		CapacityAvailable:         1,
		CleanupCapacityAvailable:  1,
		MaxOperations:             1,
		RuntimeMetadataGeneration: 1,
		InitialOperations: []OperationSnapshot{{
			Payload: operation,
		}},
		OperationStateStore: stateStore,
	}, server.Client(), provisioner.NewDummy())

	if err := agent.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitFinalized(t, service.finalized)
	observed := service.observedOperations()
	if len(observed) != 1 {
		t.Fatalf("observed operations = %d", len(observed))
	}
	if observed[0].GetCodespaceUuid() != codespaceUUID || observed[0].GetOperationRversion() != 3 {
		t.Fatalf("observed operation = %s version %d", observed[0].GetCodespaceUuid(), observed[0].GetOperationRversion())
	}
	if stateStore.savedCount() != 0 {
		t.Fatalf("saved loaded operation = %d", stateStore.savedCount())
	}
	waitDeleted(t, stateStore, 1)
	if stateStore.deletedCount() != 1 {
		t.Fatalf("deleted loaded operation = %d", stateStore.deletedCount())
	}
}

func TestAgentPausesCreateWhenLocalLeaseExpires(t *testing.T) {
	t.Parallel()

	codespaceUUID := "44444444-4444-4444-8444-444444444444"
	service := &managerService{
		finalized: make(chan struct{}, 1),
		operation: &codespacev1.OperationPayload{
			OperationRversion:         4,
			CodespaceUuid:             codespaceUUID,
			LogOffset:                 0,
			LeaseValidForMilliseconds: 200,
			Command: &codespacev1.OperationPayload_Create{
				Create: &codespacev1.CreateOperationPayload{
					RepoFullName:     "owner/repo",
					RepoCloneHttpUrl: "https://gitea.example.com/owner/repo.git",
					RepoCloneSshUrl:  "git@gitea.example.com:owner/repo.git",
					RepoTag:          "default",
					GitProtocol:      codespacev1.GitProtocol_GIT_PROTOCOL_HTTP,
					RuntimeSettings:  &codespacev1.EffectiveCodespaceRuntimeSettings{},
					CommitSha:        "0123456789abcdef0123456789abcdef01234567",
				},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	stateStore := &memoryOperationStateStore{}
	provisioner := newBlockingProvisioner()
	agent := New(AgentConfig{
		BaseURL:                   server.URL,
		ManagerID:                 7,
		ManagerSecret:             "manager-secret",
		Name:                      "test-manager",
		GatewayURL:                "https://workspace.example.net",
		GatewaySSHAddr:            "workspace.example.net:22",
		Version:                   "test",
		Tags:                      []string{"default"},
		CapacityTotal:             1,
		CapacityAvailable:         1,
		CleanupCapacityAvailable:  1,
		MaxOperations:             1,
		RuntimeMetadataGeneration: 1,
		OperationStateStore:       stateStore,
	}, server.Client(), provisioner)

	if err := agent.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitStarted(t, provisioner.started)
	waitStopped(t, provisioner.stopped)
	waitSavedStage(t, stateStore, OperationWorkerStageLeasePaused)
	if stateStore.deletedCount() != 0 {
		t.Fatalf("deleted active operations = %d", stateStore.deletedCount())
	}
	select {
	case <-service.finalized:
		t.Fatalf("operation was finalized after local lease expiry")
	default:
	}

	close(provisioner.release)
}

func TestAgentTriggersInventoryAfterResourceAbsentFinal(t *testing.T) {
	t.Parallel()

	codespaceUUID := "99999999-9999-4999-8999-999999999999"
	service := &managerService{
		finalized:           make(chan struct{}, 1),
		inventoryReported:   make(chan struct{}, 1),
		finalResourceAbsent: true,
		operation: &codespacev1.OperationPayload{
			OperationRversion:         1,
			CodespaceUuid:             codespaceUUID,
			LogOffset:                 0,
			LeaseValidForMilliseconds: 30000,
			Command: &codespacev1.OperationPayload_Create{
				Create: &codespacev1.CreateOperationPayload{
					RepoFullName:     "owner/repo",
					RepoCloneHttpUrl: "https://gitea.example.com/owner/repo.git",
					RepoCloneSshUrl:  "git@gitea.example.com:owner/repo.git",
					RepoTag:          "default",
					GitProtocol:      codespacev1.GitProtocol_GIT_PROTOCOL_HTTP,
					RuntimeSettings:  &codespacev1.EffectiveCodespaceRuntimeSettings{},
					CommitSha:        "0123456789abcdef0123456789abcdef01234567",
				},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:                   server.URL,
		ManagerID:                 7,
		ManagerSecret:             "manager-secret",
		Name:                      "test-manager",
		GatewayURL:                "https://workspace.example.net",
		GatewaySSHAddr:            "workspace.example.net:22",
		Version:                   "test",
		Tags:                      []string{"default"},
		CapacityTotal:             1,
		CapacityAvailable:         1,
		CleanupCapacityAvailable:  1,
		MaxOperations:             1,
		RuntimeMetadataGeneration: 1,
	}, server.Client(), provisioner.NewDummy())

	if err := agent.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitFinalized(t, service.finalized)
	waitInventoryReported(t, service.inventoryReported)
	waitOperationCleared(t, agent, codespaceUUID)
	_, instances := service.inventoryState()
	if len(instances) != 1 || instances[0].GetObservedOperationRversion() != 0 {
		t.Fatalf("inventory instances after resource absent = %#v", instances)
	}
}

func TestFetchStopsOnOperationVersionRegression(t *testing.T) {
	t.Parallel()

	codespaceUUID := "99999999-9999-4999-8999-999999999999"
	service := &managerService{
		operation: &codespacev1.OperationPayload{
			OperationRversion:         5,
			CodespaceUuid:             codespaceUUID,
			LogOffset:                 0,
			LeaseValidForMilliseconds: 30000,
			Command: &codespacev1.OperationPayload_Stop{
				Stop: &codespacev1.StopOperationPayload{},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:       server.URL,
		ManagerID:     7,
		ManagerSecret: "manager-secret",
		InitialOperations: []OperationSnapshot{{
			Payload: &codespacev1.OperationPayload{
				OperationRversion: 6,
				CodespaceUuid:     codespaceUUID,
				Command: &codespacev1.OperationPayload_Stop{
					Stop: &codespacev1.StopOperationPayload{},
				},
			},
		}},
	}, server.Client(), provisioner.NewDummy())

	err := agent.pollOnce(context.Background())
	if err == nil {
		t.Fatalf("expected operation version regression")
	}
	if category := failureCategory(err); category != failureOperationRegression {
		t.Fatalf("failure category = %q", category)
	}
}

func TestFetchDropsDelayedOperationVersion(t *testing.T) {
	t.Parallel()

	codespaceUUID := "99999999-9999-4999-8999-999999999999"
	agent := New(AgentConfig{
		BaseURL: "http://127.0.0.1",
		InitialOperations: []OperationSnapshot{{
			Payload: &codespacev1.OperationPayload{
				OperationRversion: 6,
				CodespaceUuid:     codespaceUUID,
				Command: &codespacev1.OperationPayload_Stop{
					Stop: &codespacev1.StopOperationPayload{},
				},
			},
		}},
	}, http.DefaultClient, provisioner.NewDummy())

	ok, err := agent.validateOperationResponseVersion("fetch operation", codespaceUUID, map[string]int64{codespaceUUID: 4}, 5)
	if err != nil {
		t.Fatalf("validate delayed version: %v", err)
	}
	if ok {
		t.Fatalf("delayed operation version was accepted")
	}
}

func TestAgentRunReportsInventoryBeforeOnline(t *testing.T) {
	t.Parallel()

	codespaceUUID := "66666666-6666-4666-8666-666666666666"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	service := &managerService{
		onlineDeclared: make(chan struct{}),
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inventoryStore := &memoryInventoryStateStore{}
	agent := New(AgentConfig{
		BaseURL:                  server.URL,
		ManagerID:                7,
		ManagerSecret:            "manager-secret",
		Name:                     "test-manager",
		GatewayURL:               "https://workspace.example.net",
		GatewaySSHAddr:           "workspace.example.net:22",
		Version:                  "test",
		Tags:                     []string{"default"},
		PollInterval:             time.Hour,
		DeclareInterval:          time.Millisecond,
		CapacityTotal:            1,
		CapacityAvailable:        1,
		CleanupCapacityAvailable: 1,
		MaxOperations:            1,
		InventoryGeneration:      3,
		InventoryStateStore:      inventoryStore,
	}, server.Client(), dummyProvisioner)

	errChannel := make(chan error, 1)
	go func() {
		errChannel <- agent.Run(ctx)
	}()

	select {
	case <-service.onlineDeclared:
	case <-time.After(time.Second):
		t.Fatalf("online declare was not reached")
	}
	cancel()
	select {
	case err := <-errChannel:
		if err != nil {
			t.Fatalf("run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("agent did not stop after context cancellation")
	}
	if service.onlineBeforeInventory() {
		t.Fatalf("online declare happened before inventory")
	}
	generations, instances := service.inventoryState()
	if len(generations) != 1 || generations[0] != 4 {
		t.Fatalf("inventory generations = %v", generations)
	}
	if len(instances) != 1 {
		t.Fatalf("inventory instances = %d", len(instances))
	}
	if instances[0].GetCodespaceUuid() != codespaceUUID ||
		instances[0].GetRuntimeState() != codespacev1.RuntimeState_RUNTIME_STATE_RUNNING ||
		instances[0].GetObservedOperationRversion() != 0 {
		t.Fatalf("inventory instance = %#v", instances[0])
	}
	if saved := inventoryStore.savedGenerations(); len(saved) != 1 || saved[0] != 4 {
		t.Fatalf("saved inventory generations = %v", saved)
	}
}

func TestReportInventoryPersistsGenerationBeforeRPCFailure(t *testing.T) {
	t.Parallel()

	service := &managerService{
		inventoryErr: connect.NewError(connect.CodeUnavailable, errors.New("temporary inventory failure")),
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	inventoryStore := &memoryInventoryStateStore{}
	agent := New(AgentConfig{
		BaseURL:             server.URL,
		ManagerID:           7,
		ManagerSecret:       "manager-secret",
		InventoryGeneration: 10,
		InventoryStateStore: inventoryStore,
	}, server.Client(), provisioner.NewDummy())

	err := agent.reportInventoryOnce(context.Background())
	if err == nil {
		t.Fatalf("expected inventory error")
	}
	generations, _ := service.inventoryState()
	if len(generations) != 1 || generations[0] != 11 {
		t.Fatalf("inventory generations = %v", generations)
	}
	if saved := inventoryStore.savedGenerations(); len(saved) != 1 || saved[0] != 11 {
		t.Fatalf("saved inventory generations = %v", saved)
	}
}

func TestReportInventoryStopsOnInventoryGenerationExhaustion(t *testing.T) {
	t.Parallel()

	service := &managerService{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:             server.URL,
		ManagerID:           7,
		ManagerSecret:       "manager-secret",
		InventoryGeneration: math.MaxInt64,
	}, server.Client(), provisioner.NewDummy())

	err := agent.reportInventoryOnce(context.Background())
	if err == nil {
		t.Fatalf("expected inventory generation exhaustion")
	}
	if category := failureCategory(err); category != failureLocalStateCommit {
		t.Fatalf("failure category = %q", category)
	}
	generations, _ := service.inventoryState()
	if len(generations) != 0 {
		t.Fatalf("inventory generations = %v", generations)
	}
}

func TestAgentRunStopsOnInventoryStateHistoryConflict(t *testing.T) {
	t.Parallel()

	service := &managerService{
		inventoryErr: testFailureError(connect.CodeFailedPrecondition, failureStateHistoryConflict),
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:                  server.URL,
		ManagerID:                7,
		ManagerSecret:            "manager-secret",
		Name:                     "test-manager",
		GatewayURL:               "https://workspace.example.net",
		GatewaySSHAddr:           "workspace.example.net:22",
		Version:                  "test",
		Tags:                     []string{"default"},
		PollInterval:             time.Hour,
		DeclareInterval:          time.Millisecond,
		CapacityTotal:            1,
		CapacityAvailable:        1,
		CleanupCapacityAvailable: 1,
		MaxOperations:            1,
	}, server.Client(), provisioner.NewDummy())

	err := agent.Run(context.Background())
	if err == nil {
		t.Fatalf("expected inventory state history conflict")
	}
	if category := failureCategory(err); category != failureStateHistoryConflict {
		t.Fatalf("failure category = %q", category)
	}
	if service.onlineWasDeclared() {
		t.Fatalf("online declare happened after inventory conflict")
	}
}

func TestReportInventoryStopsOnOperationVersionRegression(t *testing.T) {
	t.Parallel()

	codespaceUUID := "77777777-7777-4777-8777-777777777777"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	service := &managerService{
		inventoryResults: []*codespacev1.RuntimeInstanceResult{{
			CodespaceUuid: codespaceUUID,
			Action: &codespacev1.RuntimeInstanceResult_StopLocalRuntime{
				StopLocalRuntime: &codespacev1.StopLocalRuntime{
					CurrentOperationRversion: 5,
				},
			},
		}},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:             server.URL,
		ManagerID:           7,
		ManagerSecret:       "manager-secret",
		InventoryGeneration: 6,
		InitialOperations: []OperationSnapshot{{
			Payload: &codespacev1.OperationPayload{
				OperationRversion: 6,
				CodespaceUuid:     codespaceUUID,
				Command: &codespacev1.OperationPayload_Stop{
					Stop: &codespacev1.StopOperationPayload{},
				},
			},
		}},
	}, server.Client(), dummyProvisioner)

	err := agent.reportInventoryOnce(context.Background())
	if err == nil {
		t.Fatalf("expected operation version regression")
	}
	if category := failureCategory(err); category != failureOperationRegression {
		t.Fatalf("failure category = %q", category)
	}
	instances, err := dummyProvisioner.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("list dummy instances: %v", err)
	}
	if len(instances) != 1 || instances[0].RuntimeState != provisioner.RuntimeStateRunning {
		t.Fatalf("instances after regression = %#v", instances)
	}
}

func TestInventoryActionDropsDelayedOperationVersion(t *testing.T) {
	t.Parallel()

	codespaceUUID := "77777777-7777-4777-8777-777777777777"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	agent := New(AgentConfig{
		BaseURL:             "http://127.0.0.1",
		InventoryGeneration: 6,
		InitialOperations: []OperationSnapshot{{
			Payload: &codespacev1.OperationPayload{
				OperationRversion: 6,
				CodespaceUuid:     codespaceUUID,
				Command: &codespacev1.OperationPayload_Stop{
					Stop: &codespacev1.StopOperationPayload{},
				},
			},
		}},
	}, http.DefaultClient, dummyProvisioner)

	err := agent.applyInventoryResults(
		context.Background(),
		6,
		map[string]codespacev1.RuntimeState{codespaceUUID: codespacev1.RuntimeState_RUNTIME_STATE_RUNNING},
		map[string]int64{codespaceUUID: 4},
		[]*codespacev1.RuntimeInstanceResult{{
			CodespaceUuid: codespaceUUID,
			Action: &codespacev1.RuntimeInstanceResult_StopLocalRuntime{
				StopLocalRuntime: &codespacev1.StopLocalRuntime{
					CurrentOperationRversion: 5,
				},
			},
		}},
	)
	if err != nil {
		t.Fatalf("apply delayed inventory action: %v", err)
	}
	instances, err := dummyProvisioner.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("list dummy instances: %v", err)
	}
	if len(instances) != 1 || instances[0].RuntimeState != provisioner.RuntimeStateRunning {
		t.Fatalf("instances after delayed action = %#v", instances)
	}
}

func TestReportInventoryReportsStoppedRuntimeTransition(t *testing.T) {
	t.Parallel()

	codespaceUUID := "77777777-7777-4777-8777-777777777777"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	if err := dummyProvisioner.Stop(context.Background(), runtimeInstanceName(codespaceUUID)); err != nil {
		t.Fatalf("stop dummy runtime: %v", err)
	}
	service := &managerService{
		inventoryResults: []*codespacev1.RuntimeInstanceResult{{
			CodespaceUuid: codespaceUUID,
			Action: &codespacev1.RuntimeInstanceResult_ReportRuntimeTransition{
				ReportRuntimeTransition: &codespacev1.ReportRuntimeTransitionAction{
					CurrentOperationRversion: 31,
				},
			},
		}},
	}
	runtimeStore := &memoryRuntimeStateStore{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:             server.URL,
		ManagerID:           7,
		ManagerSecret:       "manager-secret",
		InventoryGeneration: 6,
		RuntimeStateStore:   runtimeStore,
	}, server.Client(), dummyProvisioner)

	if err := agent.reportInventoryOnce(context.Background()); err != nil {
		t.Fatalf("report inventory: %v", err)
	}
	transitions := service.runtimeTransitions()
	if len(transitions) != 1 {
		t.Fatalf("runtime transitions = %d", len(transitions))
	}
	transition := transitions[0]
	if transition.GetProtocolVersion() != 1 ||
		transition.GetCodespaceUuid() != codespaceUUID ||
		transition.GetRuntimeGeneration() != 1 ||
		transition.GetObservedOperationRversion() != 31 ||
		transition.GetRuntimeState() != codespacev1.RuntimeState_RUNTIME_STATE_STOPPED {
		t.Fatalf("runtime transition = %#v", transition)
	}
	generations, instances := service.inventoryState()
	if len(generations) != 1 || generations[0] != 7 {
		t.Fatalf("inventory generations = %v", generations)
	}
	if len(instances) != 1 || instances[0].GetRuntimeState() != codespacev1.RuntimeState_RUNTIME_STATE_STOPPED {
		t.Fatalf("inventory instances = %#v", instances)
	}
	if saved, cleared := runtimeStore.state(); len(saved) != 1 || len(cleared) != 1 {
		t.Fatalf("runtime store saved=%#v cleared=%#v", saved, cleared)
	}
}

func TestReportRuntimeTransitionRequiresPendingStateBeforeRPC(t *testing.T) {
	t.Parallel()

	codespaceUUID := "77777777-7777-4777-8777-777777777777"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	if err := dummyProvisioner.Stop(context.Background(), runtimeInstanceName(codespaceUUID)); err != nil {
		t.Fatalf("stop dummy runtime: %v", err)
	}
	service := &managerService{
		inventoryResults: []*codespacev1.RuntimeInstanceResult{{
			CodespaceUuid: codespaceUUID,
			Action: &codespacev1.RuntimeInstanceResult_ReportRuntimeTransition{
				ReportRuntimeTransition: &codespacev1.ReportRuntimeTransitionAction{
					CurrentOperationRversion: 31,
				},
			},
		}},
	}
	runtimeStore := &memoryRuntimeStateStore{
		saveErr: errors.New("persist failed"),
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:             server.URL,
		ManagerID:           7,
		ManagerSecret:       "manager-secret",
		InventoryGeneration: 6,
		RuntimeStateStore:   runtimeStore,
	}, server.Client(), dummyProvisioner)

	if err := agent.reportInventoryOnce(context.Background()); err == nil {
		t.Fatalf("expected pending state error")
	}
	if transitions := service.runtimeTransitions(); len(transitions) != 0 {
		t.Fatalf("runtime transitions = %#v", transitions)
	}
}

func TestReportRuntimeTransitionRetriesLoadedPendingGeneration(t *testing.T) {
	t.Parallel()

	codespaceUUID := "77777777-7777-4777-8777-777777777777"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	if err := dummyProvisioner.Stop(context.Background(), runtimeInstanceName(codespaceUUID)); err != nil {
		t.Fatalf("stop dummy runtime: %v", err)
	}
	service := &managerService{
		inventoryResults: []*codespacev1.RuntimeInstanceResult{{
			CodespaceUuid: codespaceUUID,
			Action: &codespacev1.RuntimeInstanceResult_ReportRuntimeTransition{
				ReportRuntimeTransition: &codespacev1.ReportRuntimeTransitionAction{
					CurrentOperationRversion: 31,
				},
			},
		}},
	}
	runtimeStore := &memoryRuntimeStateStore{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:             server.URL,
		ManagerID:           7,
		ManagerSecret:       "manager-secret",
		InventoryGeneration: 6,
		InitialRuntimeTransitions: []RuntimeTransitionSnapshot{{
			CodespaceUUID:             codespaceUUID,
			TargetState:               codespacev1.RuntimeState_RUNTIME_STATE_STOPPED,
			RuntimeGeneration:         5,
			ObservedOperationRVersion: 31,
		}},
		RuntimeStateStore: runtimeStore,
	}, server.Client(), dummyProvisioner)

	if err := agent.reportInventoryOnce(context.Background()); err != nil {
		t.Fatalf("report inventory: %v", err)
	}
	transitions := service.runtimeTransitions()
	if len(transitions) != 1 || transitions[0].GetRuntimeGeneration() != 5 {
		t.Fatalf("runtime transitions = %#v", transitions)
	}
	saved, cleared := runtimeStore.state()
	if len(saved) != 0 || len(cleared) != 1 || cleared[0] != 5 {
		t.Fatalf("runtime store saved=%#v cleared=%#v", saved, cleared)
	}
}

func TestReportInventoryPersistsCleanupPendingBeforeDelete(t *testing.T) {
	t.Parallel()

	codespaceUUID := "88888888-8888-4888-8888-888888888888"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	service := &managerService{
		inventoryResults: []*codespacev1.RuntimeInstanceResult{{
			CodespaceUuid: codespaceUUID,
			Action: &codespacev1.RuntimeInstanceResult_CleanupLocalRuntime{
				CleanupLocalRuntime: &codespacev1.CleanupLocalRuntime{},
			},
		}},
	}
	cleanupStore := &memoryCleanupStateStore{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:             server.URL,
		ManagerID:           7,
		ManagerSecret:       "manager-secret",
		InventoryGeneration: 6,
		CleanupStateStore:   cleanupStore,
	}, server.Client(), dummyProvisioner)

	if err := agent.reportInventoryOnce(context.Background()); err != nil {
		t.Fatalf("report inventory: %v", err)
	}
	saved, cleared := cleanupStore.state()
	if len(saved) != 1 || saved[0] != codespaceUUID || len(cleared) != 1 || cleared[0] != codespaceUUID {
		t.Fatalf("cleanup store saved=%#v cleared=%#v", saved, cleared)
	}
	instances, err := dummyProvisioner.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("list dummy instances: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("instances after cleanup = %#v", instances)
	}
}

func TestReportInventoryRequiresCleanupPendingBeforeDelete(t *testing.T) {
	t.Parallel()

	codespaceUUID := "88888888-8888-4888-8888-888888888888"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	service := &managerService{
		inventoryResults: []*codespacev1.RuntimeInstanceResult{{
			CodespaceUuid: codespaceUUID,
			Action: &codespacev1.RuntimeInstanceResult_CleanupLocalRuntime{
				CleanupLocalRuntime: &codespacev1.CleanupLocalRuntime{},
			},
		}},
	}
	cleanupStore := &memoryCleanupStateStore{
		saveErr: errors.New("persist cleanup failed"),
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:             server.URL,
		ManagerID:           7,
		ManagerSecret:       "manager-secret",
		InventoryGeneration: 6,
		CleanupStateStore:   cleanupStore,
	}, server.Client(), dummyProvisioner)

	if err := agent.reportInventoryOnce(context.Background()); err == nil {
		t.Fatalf("expected cleanup pending error")
	}
	instances, err := dummyProvisioner.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("list dummy instances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("instances after failed cleanup pending = %#v", instances)
	}
}

func TestAgentRunsLoadedCleanupPending(t *testing.T) {
	t.Parallel()

	codespaceUUID := "88888888-8888-4888-8888-888888888888"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	cleanupStore := &memoryCleanupStateStore{}
	agent := New(AgentConfig{
		BaseURL:                "http://127.0.0.1",
		InitialCleanupPendings: []string{codespaceUUID},
		CleanupStateStore:      cleanupStore,
	}, http.DefaultClient, dummyProvisioner)

	if err := agent.runCleanupPendings(context.Background()); err != nil {
		t.Fatalf("run cleanup pendings: %v", err)
	}
	_, cleared := cleanupStore.state()
	if len(cleared) != 1 || cleared[0] != codespaceUUID {
		t.Fatalf("cleanup store cleared=%#v", cleared)
	}
	instances, err := dummyProvisioner.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("list dummy instances: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("instances after loaded cleanup = %#v", instances)
	}
}

func TestCleanupLocalRuntimeKeepsStateUntilInstanceIsAbsent(t *testing.T) {
	t.Parallel()

	codespaceUUID := "88888888-8888-4888-8888-888888888888"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	cleanupStore := &memoryCleanupStateStore{}
	agent := New(AgentConfig{
		BaseURL:           "http://127.0.0.1",
		CleanupStateStore: cleanupStore,
	}, http.DefaultClient, &nonDeletingProvisioner{base: dummyProvisioner})

	if err := agent.cleanupLocalRuntime(context.Background(), codespaceUUID); err == nil {
		t.Fatalf("expected cleanup confirmation error")
	}
	_, cleared := cleanupStore.state()
	if len(cleared) != 0 {
		t.Fatalf("cleanup state cleared before instance absence: %#v", cleared)
	}
}

func TestDeleteOperationPersistsCleanupPendingBeforeDelete(t *testing.T) {
	t.Parallel()

	codespaceUUID := "88888888-8888-4888-8888-888888888888"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	service := &managerService{finalized: make(chan struct{}, 1)}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	cleanupStore := &memoryCleanupStateStore{}
	agent := New(AgentConfig{
		BaseURL:           server.URL,
		ManagerID:         7,
		ManagerSecret:     "manager-secret",
		CleanupStateStore: cleanupStore,
	}, server.Client(), dummyProvisioner)
	operation := &codespacev1.OperationPayload{
		OperationRversion: 9,
		CodespaceUuid:     codespaceUUID,
		Command: &codespacev1.OperationPayload_Delete{
			Delete: &codespacev1.DeleteOperationPayload{},
		},
	}

	if err := agent.handleOperation(context.Background(), operation); err != nil {
		t.Fatalf("handle delete operation: %v", err)
	}
	waitFinalized(t, service.finalized)
	saved, cleared := cleanupStore.state()
	if len(saved) != 1 || saved[0] != codespaceUUID || len(cleared) != 1 || cleared[0] != codespaceUUID {
		t.Fatalf("cleanup store saved=%#v cleared=%#v", saved, cleared)
	}
	instances, err := dummyProvisioner.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("list dummy instances: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("instances after delete operation = %#v", instances)
	}
}

func TestDeleteOperationRequiresCleanupPendingBeforeDelete(t *testing.T) {
	t.Parallel()

	codespaceUUID := "88888888-8888-4888-8888-888888888888"
	dummyProvisioner := provisioner.NewDummy()
	if _, err := dummyProvisioner.CreateOrStart(context.Background(), provisioner.InstanceSpec{
		CodespaceUUID: codespaceUUID,
		Name:          runtimeInstanceName(codespaceUUID),
		RepoFullName:  "owner/repo",
		RepoTag:       "default",
	}); err != nil {
		t.Fatalf("create dummy runtime: %v", err)
	}
	service := &managerService{finalized: make(chan struct{}, 1)}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:       server.URL,
		ManagerID:     7,
		ManagerSecret: "manager-secret",
		CleanupStateStore: &memoryCleanupStateStore{
			saveErr: errors.New("persist cleanup failed"),
		},
	}, server.Client(), dummyProvisioner)
	operation := &codespacev1.OperationPayload{
		OperationRversion: 9,
		CodespaceUuid:     codespaceUUID,
		Command: &codespacev1.OperationPayload_Delete{
			Delete: &codespacev1.DeleteOperationPayload{},
		},
	}

	err := agent.handleOperation(context.Background(), operation)
	if err == nil {
		t.Fatalf("expected cleanup pending error")
	}
	if category := failureCategory(err); category != failureLocalStateCommit {
		t.Fatalf("failure category = %q", category)
	}
	select {
	case <-service.finalized:
		t.Fatalf("delete operation was finalized after cleanup pending failure")
	default:
	}
	instances, err := dummyProvisioner.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("list dummy instances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("instances after cleanup pending failure = %#v", instances)
	}
}

func TestRequestIdleStopPending(t *testing.T) {
	t.Parallel()

	codespaceUUID := "99999999-9999-4999-8999-999999999999"
	service := &managerService{
		idleStopResponse: &codespacev1.RequestIdleStopResponse{
			Outcome: &codespacev1.RequestIdleStopResponse_Pending{
				Pending: &codespacev1.IdleStopPending{OperationRversion: 12},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:       server.URL,
		ManagerID:     7,
		ManagerSecret: "manager-secret",
	}, server.Client(), provisioner.NewDummy())
	result, err := agent.requestIdleStop(context.Background(), codespaceUUID, &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       true,
		IdleTimeoutSeconds:    1800,
		InteractionGeneration: 33,
	})
	if err != nil {
		t.Fatalf("request idle stop: %v", err)
	}
	if result.outcome != idleStopOutcomePending || result.operationRVersion != 12 {
		t.Fatalf("idle stop result = %#v", result)
	}
	requests := service.idleStopRequests()
	if len(requests) != 1 {
		t.Fatalf("idle stop requests = %d", len(requests))
	}
	request := requests[0]
	if request.GetProtocolVersion() != 1 ||
		request.GetCodespaceUuid() != codespaceUUID ||
		!request.GetObservedAutoStopEnabled() ||
		request.GetObservedIdleTimeoutSeconds() != 1800 ||
		request.GetObservedInteractionGeneration() != 33 {
		t.Fatalf("idle stop request = %#v", request)
	}
}

func TestRequestIdleStopObservationChanged(t *testing.T) {
	t.Parallel()

	service := &managerService{
		idleStopResponse: &codespacev1.RequestIdleStopResponse{
			Outcome: &codespacev1.RequestIdleStopResponse_ObservationChanged{
				ObservationChanged: &codespacev1.IdleStopObservationChanged{
					RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{
						AutoStopEnabled:       false,
						IdleTimeoutSeconds:    0,
						InteractionGeneration: 34,
					},
				},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:       server.URL,
		ManagerID:     7,
		ManagerSecret: "manager-secret",
	}, server.Client(), provisioner.NewDummy())
	result, err := agent.requestIdleStop(context.Background(), "99999999-9999-4999-8999-999999999999", &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       true,
		IdleTimeoutSeconds:    1800,
		InteractionGeneration: 33,
	})
	if err != nil {
		t.Fatalf("request idle stop: %v", err)
	}
	if result.outcome != idleStopOutcomeObservationChanged ||
		result.runtimeSettings.GetAutoStopEnabled() ||
		result.runtimeSettings.GetInteractionGeneration() != 34 {
		t.Fatalf("idle stop result = %#v", result)
	}
}

func TestRequestIdleStopNotApplicable(t *testing.T) {
	t.Parallel()

	service := &managerService{
		idleStopResponse: &codespacev1.RequestIdleStopResponse{
			Outcome: &codespacev1.RequestIdleStopResponse_NotApplicable{
				NotApplicable: &codespacev1.IdleStopNotApplicable{
					Reason: codespacev1.IdleStopNotApplicableReason_IDLE_STOP_NOT_APPLICABLE_REASON_OPERATION_CONFLICT,
				},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:       server.URL,
		ManagerID:     7,
		ManagerSecret: "manager-secret",
	}, server.Client(), provisioner.NewDummy())
	result, err := agent.requestIdleStop(context.Background(), "99999999-9999-4999-8999-999999999999", &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       true,
		IdleTimeoutSeconds:    1800,
		InteractionGeneration: 33,
	})
	if err != nil {
		t.Fatalf("request idle stop: %v", err)
	}
	if result.outcome != idleStopOutcomeNotApplicable ||
		result.notApplicable != codespacev1.IdleStopNotApplicableReason_IDLE_STOP_NOT_APPLICABLE_REASON_OPERATION_CONFLICT {
		t.Fatalf("idle stop result = %#v", result)
	}
}

func TestAutoStopRequestsIdleStopAfterTimeout(t *testing.T) {
	t.Parallel()

	codespaceUUID := "99999999-9999-4999-8999-999999999999"
	service := &managerService{
		idleStopResponse: &codespacev1.RequestIdleStopResponse{
			Outcome: &codespacev1.RequestIdleStopResponse_Pending{
				Pending: &codespacev1.IdleStopPending{OperationRversion: 12},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:       server.URL,
		ManagerID:     7,
		ManagerSecret: "manager-secret",
	}, server.Client(), provisioner.NewDummy())
	now := time.Now()
	agent.applyRuntimeSettings(codespaceUUID, &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       true,
		IdleTimeoutSeconds:    1,
		InteractionGeneration: 1,
	}, now)
	agent.markRuntimeReady(codespaceUUID)
	agent.autoStopMu.Lock()
	agent.autoStops[codespaceUUID].idleStarted = now.Add(-2 * time.Second)
	agent.autoStopMu.Unlock()

	if err := agent.reconcileAutoStops(context.Background()); err != nil {
		t.Fatalf("reconcile auto stops: %v", err)
	}
	requests := service.idleStopRequests()
	if len(requests) != 1 {
		t.Fatalf("idle stop requests = %d", len(requests))
	}
	if requests[0].GetCodespaceUuid() != codespaceUUID ||
		!requests[0].GetObservedAutoStopEnabled() ||
		requests[0].GetObservedIdleTimeoutSeconds() != 1 ||
		requests[0].GetObservedInteractionGeneration() != 1 {
		t.Fatalf("idle stop request = %#v", requests[0])
	}
	agent.autoStopMu.Lock()
	pendingVersion := agent.autoStops[codespaceUUID].pendingVersion
	agent.autoStopMu.Unlock()
	if pendingVersion != 12 {
		t.Fatalf("pending idle stop version = %d", pendingVersion)
	}
}

func TestAutoStopInteractionGenerationRestartsIdleWindow(t *testing.T) {
	t.Parallel()

	codespaceUUID := "99999999-9999-4999-8999-999999999999"
	agent := New(AgentConfig{BaseURL: "http://127.0.0.1"}, http.DefaultClient, provisioner.NewDummy())
	started := time.Unix(100, 0)
	agent.applyRuntimeSettings(codespaceUUID, &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       true,
		IdleTimeoutSeconds:    10,
		InteractionGeneration: 1,
	}, started)
	agent.markRuntimeReady(codespaceUUID)
	agent.autoStopMu.Lock()
	agent.autoStops[codespaceUUID].idleStarted = started
	agent.autoStopMu.Unlock()

	interaction := started.Add(20 * time.Second)
	agent.applyRuntimeSettings(codespaceUUID, &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       true,
		IdleTimeoutSeconds:    10,
		InteractionGeneration: 2,
	}, interaction)

	if requests := agent.dueAutoStopRequests(interaction.Add(9 * time.Second)); len(requests) != 0 {
		t.Fatalf("auto stop requests before restarted timeout = %#v", requests)
	}
	requests := agent.dueAutoStopRequests(interaction.Add(10 * time.Second))
	if len(requests) != 1 || requests[0].settings.GetInteractionGeneration() != 2 {
		t.Fatalf("auto stop requests after restarted timeout = %#v", requests)
	}
}

func TestAutoStopSkipsActiveOperation(t *testing.T) {
	t.Parallel()

	codespaceUUID := "99999999-9999-4999-8999-999999999999"
	agent := New(AgentConfig{BaseURL: "http://127.0.0.1"}, http.DefaultClient, provisioner.NewDummy())
	now := time.Now()
	agent.applyRuntimeSettings(codespaceUUID, &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       true,
		IdleTimeoutSeconds:    1,
		InteractionGeneration: 1,
	}, now)
	agent.markRuntimeReady(codespaceUUID)
	agent.activeOperations[codespaceUUID] = &operationContext{
		operationRVersion: 3,
		payload: &codespacev1.OperationPayload{
			CodespaceUuid:     codespaceUUID,
			OperationRversion: 3,
		},
	}
	agent.autoStopMu.Lock()
	agent.autoStops[codespaceUUID].idleStarted = now.Add(-2 * time.Second)
	agent.autoStopMu.Unlock()

	if requests := agent.dueAutoStopRequests(now); len(requests) != 0 {
		t.Fatalf("auto stop requests with active operation = %#v", requests)
	}
}

func TestAutoStopSkipsLiveSession(t *testing.T) {
	t.Parallel()

	codespaceUUID := "99999999-9999-4999-8999-999999999999"
	agent := New(AgentConfig{
		BaseURL:        "http://127.0.0.1",
		SessionTracker: staticSessionTracker{codespaceUUID: 1},
	}, http.DefaultClient, provisioner.NewDummy())
	now := time.Now()
	agent.applyRuntimeSettings(codespaceUUID, &codespacev1.EffectiveCodespaceRuntimeSettings{
		AutoStopEnabled:       true,
		IdleTimeoutSeconds:    1,
		InteractionGeneration: 1,
	}, now)
	agent.markRuntimeReady(codespaceUUID)
	agent.autoStopMu.Lock()
	agent.autoStops[codespaceUUID].idleStarted = now.Add(-2 * time.Second)
	agent.autoStopMu.Unlock()

	if requests := agent.dueAutoStopRequests(now); len(requests) != 0 {
		t.Fatalf("auto stop requests with live session = %#v", requests)
	}
}

func TestAgentRunRetriesTransientFetchError(t *testing.T) {
	t.Parallel()

	service := &transientFetchService{
		fetchedTwice: make(chan struct{}),
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := New(AgentConfig{
		BaseURL:                  server.URL,
		ManagerID:                7,
		ManagerSecret:            "manager-secret",
		Name:                     "test-manager",
		GatewayURL:               "https://workspace.example.net",
		GatewaySSHAddr:           "workspace.example.net:22",
		Version:                  "test",
		Tags:                     []string{"default"},
		PollInterval:             time.Millisecond,
		DeclareInterval:          time.Hour,
		CapacityTotal:            1,
		CapacityAvailable:        1,
		CleanupCapacityAvailable: 1,
		MaxOperations:            1,
	}, server.Client(), provisioner.NewDummy())

	errChannel := make(chan error, 1)
	go func() {
		errChannel <- agent.Run(ctx)
	}()

	select {
	case <-service.fetchedTwice:
	case <-time.After(time.Second):
		t.Fatalf("transient fetch error was not retried")
	}
	cancel()
	select {
	case err := <-errChannel:
		if err != nil {
			t.Fatalf("run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("agent did not stop after context cancellation")
	}
}

func TestAgentRunStopsOnProtocolMismatch(t *testing.T) {
	t.Parallel()

	service := &protocolMismatchService{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:                  server.URL,
		ManagerID:                7,
		ManagerSecret:            "manager-secret",
		Name:                     "test-manager",
		GatewayURL:               "https://workspace.example.net",
		GatewaySSHAddr:           "workspace.example.net:22",
		Version:                  "test",
		Tags:                     []string{"default"},
		PollInterval:             time.Millisecond,
		DeclareInterval:          time.Millisecond,
		CapacityTotal:            1,
		CapacityAvailable:        1,
		CleanupCapacityAvailable: 1,
		MaxOperations:            1,
	}, server.Client(), provisioner.NewDummy())

	err := agent.Run(context.Background())
	if err == nil {
		t.Fatalf("expected protocol mismatch error")
	}
	if category := failureCategory(err); category != failureProtocolMismatch {
		t.Fatalf("failure category = %q", category)
	}
}

func TestAgentDeclareSavesManagerServiceSettings(t *testing.T) {
	t.Parallel()

	service := &managerService{}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	store := &memoryManagerServiceSettingsStore{}
	agent := New(AgentConfig{
		BaseURL:                server.URL,
		ManagerID:              7,
		ManagerSecret:          "manager-secret",
		Name:                   "test-manager",
		GatewayURL:             "https://workspace.example.net",
		GatewaySSHAddr:         "workspace.example.net:22",
		Version:                "test",
		Tags:                   []string{"default"},
		CapacityTotal:          1,
		CapacityAvailable:      1,
		ManagerServiceSettings: store,
	}, server.Client(), provisioner.NewDummy())

	if err := agent.declare(context.Background(), codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_RECOVERING); err != nil {
		t.Fatalf("declare: %v", err)
	}
	settings, ok := store.settings()
	if !ok {
		t.Fatalf("manager service settings were not saved")
	}
	if settings.HeartbeatInterval != time.Second ||
		settings.RuntimeMetadataRefreshInterval != time.Second ||
		settings.ControlPlaneMaxMessageSize != 1<<20 ||
		settings.GiteaWebURL != "https://gitea.example.com/" {
		t.Fatalf("manager service settings = %#v", settings)
	}
}

func TestValidateDeclareResponseGiteaWebURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		giteaWebURL string
		wantErr     bool
	}{
		{name: "root", giteaWebURL: "https://gitea.example.com/"},
		{name: "app sub url", giteaWebURL: "https://gitea.example.com/git/"},
		{name: "http", giteaWebURL: "http://gitea.example.com/"},
		{name: "missing slash", giteaWebURL: "https://gitea.example.com", wantErr: true},
		{name: "missing host", giteaWebURL: "https:///git/", wantErr: true},
		{name: "userinfo", giteaWebURL: "https://user@gitea.example.com/", wantErr: true},
		{name: "query", giteaWebURL: "https://gitea.example.com/?x=1", wantErr: true},
		{name: "fragment", giteaWebURL: "https://gitea.example.com/#top", wantErr: true},
		{name: "unsupported scheme", giteaWebURL: "ssh://gitea.example.com/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := validDeclareResponse()
			response.GiteaWebUrl = tt.giteaWebURL
			_, err := validateDeclareResponse(response)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validate declare response: %v", err)
			}
		})
	}
}

func TestAgentRunStopsOnWorkerProtocolMismatch(t *testing.T) {
	t.Parallel()

	stateStore := &memoryOperationStateStore{}
	service := &managerService{
		finalized: make(chan struct{}, 1),
		tokenErr:  testFailureError(connect.CodeFailedPrecondition, failureProtocolMismatch),
		operation: &codespacev1.OperationPayload{
			OperationRversion:         5,
			CodespaceUuid:             "55555555-5555-4555-8555-555555555555",
			LogOffset:                 0,
			LeaseValidForMilliseconds: 30000,
			Command: &codespacev1.OperationPayload_Create{
				Create: &codespacev1.CreateOperationPayload{
					RepoFullName:     "owner/repo",
					RepoCloneHttpUrl: "https://gitea.example.com/owner/repo.git",
					RepoCloneSshUrl:  "git@gitea.example.com:owner/repo.git",
					RepoTag:          "default",
					GitProtocol:      codespacev1.GitProtocol_GIT_PROTOCOL_HTTP,
					RuntimeSettings:  &codespacev1.EffectiveCodespaceRuntimeSettings{},
					CommitSha:        "0123456789abcdef0123456789abcdef01234567",
				},
			},
		},
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	agent := New(AgentConfig{
		BaseURL:                   server.URL,
		ManagerID:                 7,
		ManagerSecret:             "manager-secret",
		Name:                      "test-manager",
		GatewayURL:                "https://workspace.example.net",
		GatewaySSHAddr:            "workspace.example.net:22",
		Version:                   "test",
		Tags:                      []string{"default"},
		PollInterval:              time.Millisecond,
		DeclareInterval:           time.Hour,
		CapacityTotal:             1,
		CapacityAvailable:         1,
		CleanupCapacityAvailable:  1,
		MaxOperations:             1,
		RuntimeMetadataGeneration: 1,
		OperationStateStore:       stateStore,
	}, server.Client(), provisioner.NewDummy())

	errChannel := make(chan error, 1)
	go func() {
		errChannel <- agent.Run(context.Background())
	}()

	var err error
	select {
	case err = <-errChannel:
	case <-time.After(time.Second):
		sawFetch, sawToken := service.callState()
		t.Fatalf("agent did not stop after worker protocol mismatch, sawFetch=%v sawToken=%v saved=%d stage=%q", sawFetch, sawToken, stateStore.savedCount(), stateStore.savedStage())
	}
	if err == nil {
		t.Fatalf("expected protocol mismatch error")
	}
	if category := failureCategory(err); category != failureProtocolMismatch {
		t.Fatalf("failure category = %q", category)
	}
	waitSavedStage(t, stateStore, OperationWorkerStageLeasePaused)
	select {
	case <-service.finalized:
		t.Fatalf("operation was finalized after worker protocol mismatch")
	default:
	}
}

func TestRuntimeInstanceNameUsesShortUUID(t *testing.T) {
	t.Parallel()

	name := runtimeInstanceName("11111111-2222-4333-8444-555555555555")
	if name != "cs-11111111222243338444" {
		t.Fatalf("runtime instance name = %q", name)
	}
}

type managerService struct {
	codespacev1connect.UnimplementedManagerServiceHandler

	mu                        sync.Mutex
	operation                 *codespacev1.OperationPayload
	sawDeclare                bool
	sawFetch                  bool
	sawToken                  bool
	sawMetadata               bool
	finalStatus               codespacev1.FinalStatus
	finalOperationType        codespacev1.OperationType
	metadataGeneration        int64
	metadataGenerations       []int64
	metadataOperationRVersion int64
	metadataStages            []string
	managerID                 string
	managerSecret             string
	observed                  []*codespacev1.ObservedOperation
	finalized                 chan struct{}
	inventoryReported         chan struct{}
	finalResourceAbsent       bool
	renewObserved             bool
	tokenErr                  error
	inventoryErr              error
	transitionErr             error
	inventory                 []*codespacev1.RuntimeInstanceRef
	inventoryGen              []int64
	inventoryResults          []*codespacev1.RuntimeInstanceResult
	transitions               []*codespacev1.ReportRuntimeTransitionRequest
	idleStopResponse          *codespacev1.RequestIdleStopResponse
	idleStop                  []*codespacev1.RequestIdleStopRequest
	onlineDeclared            chan struct{}
	onlineBeforeInv           bool
	sawOnline                 bool
}

type staticSessionTracker map[string]int

func (t staticSessionTracker) LiveSessions(codespaceUUID string) int {
	return t[codespaceUUID]
}

type memoryManagerServiceSettingsStore struct {
	mu    sync.Mutex
	value ManagerServiceSettings
	ok    bool
}

func (s *memoryManagerServiceSettingsStore) SaveManagerServiceSettings(settings ManagerServiceSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = settings
	s.ok = true
	return nil
}

func (s *memoryManagerServiceSettingsStore) settings() (ManagerServiceSettings, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, s.ok
}

func (s *managerService) DeclareManager(
	_ context.Context,
	req *connect.Request[codespacev1.DeclareManagerRequest],
) (*connect.Response[codespacev1.DeclareManagerResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.captureAuth(req.Header())
	s.sawDeclare = true
	if req.Msg.GetManagerRuntimeState() == codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE {
		s.sawOnline = true
		if len(s.inventoryGen) == 0 {
			s.onlineBeforeInv = true
		}
		if s.onlineDeclared != nil {
			select {
			case <-s.onlineDeclared:
			default:
				close(s.onlineDeclared)
			}
		}
	}
	if req.Msg.GetProtocolVersion() != 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	return connect.NewResponse(&codespacev1.DeclareManagerResponse{
		HeartbeatIntervalMilliseconds:              1000,
		RuntimeMetadataRefreshIntervalMilliseconds: 1000,
		ControlPlaneMaxMessageSizeBytes:            1 << 20,
		GiteaWebUrl:                                "https://gitea.example.com/",
	}), nil
}

func (s *managerService) FetchOperations(
	_ context.Context,
	req *connect.Request[codespacev1.FetchOperationsRequest],
) (*connect.Response[codespacev1.FetchOperationsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.captureAuth(req.Header())
	s.sawFetch = true
	s.observed = append([]*codespacev1.ObservedOperation(nil), req.Msg.GetObservedOperations()...)
	if req.Msg.GetProtocolVersion() != 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	operation := s.operation
	s.operation = nil
	if operation == nil {
		if s.renewObserved && len(req.Msg.GetObservedOperations()) > 0 {
			leases := make([]*codespacev1.RenewedOperationLease, 0, len(req.Msg.GetObservedOperations()))
			for _, observed := range req.Msg.GetObservedOperations() {
				leases = append(leases, &codespacev1.RenewedOperationLease{
					CodespaceUuid:             observed.GetCodespaceUuid(),
					OperationRversion:         observed.GetOperationRversion(),
					LeaseValidForMilliseconds: 30000,
				})
			}
			return connect.NewResponse(&codespacev1.FetchOperationsResponse{RenewedLeases: leases}), nil
		}
		return connect.NewResponse(&codespacev1.FetchOperationsResponse{}), nil
	}
	return connect.NewResponse(&codespacev1.FetchOperationsResponse{
		Operations: []*codespacev1.OperationPayload{operation},
	}), nil
}

func (s *managerService) ReportInstances(
	_ context.Context,
	req *connect.Request[codespacev1.ReportInstancesRequest],
) (*connect.Response[codespacev1.ReportInstancesResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.captureAuth(req.Header())
	s.inventoryGen = append(s.inventoryGen, req.Msg.GetInventoryGeneration())
	s.inventory = append([]*codespacev1.RuntimeInstanceRef(nil), req.Msg.GetInstances()...)
	if s.inventoryReported != nil {
		select {
		case s.inventoryReported <- struct{}{}:
		default:
		}
	}
	if s.inventoryErr != nil {
		return nil, s.inventoryErr
	}
	if s.inventoryResults != nil {
		return connect.NewResponse(&codespacev1.ReportInstancesResponse{
			Results: append([]*codespacev1.RuntimeInstanceResult(nil), s.inventoryResults...),
		}), nil
	}
	results := make([]*codespacev1.RuntimeInstanceResult, 0, len(req.Msg.GetInstances()))
	for _, instance := range req.Msg.GetInstances() {
		results = append(results, &codespacev1.RuntimeInstanceResult{
			CodespaceUuid: instance.GetCodespaceUuid(),
		})
	}
	return connect.NewResponse(&codespacev1.ReportInstancesResponse{Results: results}), nil
}

func (s *managerService) ReportRuntimeTransition(
	_ context.Context,
	req *connect.Request[codespacev1.ReportRuntimeTransitionRequest],
) (*connect.Response[codespacev1.ReportRuntimeTransitionResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.captureAuth(req.Header())
	transition := *req.Msg
	s.transitions = append(s.transitions, &transition)
	if s.transitionErr != nil {
		return nil, s.transitionErr
	}
	return connect.NewResponse(&codespacev1.ReportRuntimeTransitionResponse{}), nil
}

func (s *managerService) UpdateLog(
	_ context.Context,
	req *connect.Request[codespacev1.UpdateLogRequest],
) (*connect.Response[codespacev1.UpdateLogResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.captureAuth(req.Header())
	return connect.NewResponse(&codespacev1.UpdateLogResponse{
		NextOffset: req.Msg.GetOffset() + 1,
	}), nil
}

func (s *managerService) RequestGiteaToken(
	_ context.Context,
	req *connect.Request[codespacev1.RequestGiteaTokenRequest],
) (*connect.Response[codespacev1.RequestGiteaTokenResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.captureAuth(req.Header())
	s.sawToken = true
	if s.tokenErr != nil {
		return nil, s.tokenErr
	}
	return connect.NewResponse(&codespacev1.RequestGiteaTokenResponse{
		Token:     "gcs_test",
		ServerUrl: "https://gitea.example.com/",
	}), nil
}

func (s *managerService) runtimeMetadataStages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.metadataStages...)
}

func expectedRuntimeBootStages() []string {
	return []string{
		RuntimeBootStagePrepareRuntime,
		RuntimeBootStageInitializeSystem,
		RuntimeBootStagePrepareWorkspace,
		RuntimeBootStageStartEnvironment,
		RuntimeBootStagePublishRuntime,
		RuntimeBootStageReady,
	}
}

func (s *managerService) RequestIdleStop(
	_ context.Context,
	req *connect.Request[codespacev1.RequestIdleStopRequest],
) (*connect.Response[codespacev1.RequestIdleStopResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.captureAuth(req.Header())
	idleStop := *req.Msg
	s.idleStop = append(s.idleStop, &idleStop)
	if s.idleStopResponse != nil {
		return connect.NewResponse(s.idleStopResponse), nil
	}
	return connect.NewResponse(&codespacev1.RequestIdleStopResponse{}), nil
}

func (s *managerService) ReportRuntimeMetadata(
	_ context.Context,
	req *connect.Request[codespacev1.ReportRuntimeMetadataRequest],
) (*connect.Response[codespacev1.ReportRuntimeMetadataResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.captureAuth(req.Header())
	s.sawMetadata = true
	s.metadataGeneration = req.Msg.GetMetadataGeneration()
	s.metadataGenerations = append(s.metadataGenerations, req.Msg.GetMetadataGeneration())
	if req.Msg.GetMetadataGeneration() <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	var metadata struct {
		Runtime struct {
			InternalSSH struct {
				Host               string `json:"host"`
				Port               int    `json:"port"`
				User               string `json:"user"`
				AuthMode           string `json:"auth_mode"`
				HostKeyFingerprint string `json:"host_key_fingerprint"`
			} `json:"internal_ssh"`
		} `json:"runtime"`
		Endpoints []struct {
			EndpointID string `json:"endpoint_id"`
			Label      string `json:"label"`
			Public     bool   `json:"public"`
		} `json:"endpoints"`
		Boot struct {
			OperationRVersion int64  `json:"operation_rversion"`
			Stage             string `json:"stage"`
			StartedUnix       int64  `json:"started_unix"`
			LastUpdateUnix    int64  `json:"last_update_unix"`
		} `json:"boot"`
		Workspace any `json:"workspace"`
	}
	if err := json.Unmarshal([]byte(req.Msg.GetMetadataJson()), &metadata); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	s.metadataStages = append(s.metadataStages, metadata.Boot.Stage)
	expectedOperationRVersion := s.metadataOperationRVersion
	if metadata.Workspace != nil ||
		metadata.Runtime.InternalSSH.Host == "" ||
		metadata.Runtime.InternalSSH.Port <= 0 ||
		metadata.Runtime.InternalSSH.User == "" ||
		metadata.Runtime.InternalSSH.AuthMode != "publickey" ||
		metadata.Runtime.InternalSSH.HostKeyFingerprint == "" ||
		metadata.Endpoints == nil ||
		!IsRuntimeBootStage(metadata.Boot.Stage) ||
		metadata.Boot.StartedUnix <= 0 ||
		metadata.Boot.LastUpdateUnix < metadata.Boot.StartedUnix {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	if expectedOperationRVersion > 0 && metadata.Boot.OperationRVersion != expectedOperationRVersion {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	return connect.NewResponse(&codespacev1.ReportRuntimeMetadataResponse{}), nil
}

func (s *managerService) FinalizeOperation(
	_ context.Context,
	req *connect.Request[codespacev1.FinalizeOperationRequest],
) (*connect.Response[codespacev1.FinalizeOperationResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.captureAuth(req.Header())
	s.finalStatus = req.Msg.GetFinal().GetStatus()
	s.finalOperationType = req.Msg.GetFinal().GetOperationType()
	if s.finalized != nil {
		select {
		case s.finalized <- struct{}{}:
		default:
		}
	}
	if s.finalResourceAbsent {
		return connect.NewResponse(&codespacev1.FinalizeOperationResponse{
			Outcome: &codespacev1.FinalizeOperationResponse_ResourceAbsent{
				ResourceAbsent: &codespacev1.ResourceAbsent{},
			},
		}), nil
	}
	return connect.NewResponse(&codespacev1.FinalizeOperationResponse{
		Outcome: &codespacev1.FinalizeOperationResponse_FinalAccepted{
			FinalAccepted: &codespacev1.FinalAccepted{},
		},
	}), nil
}

func (s *managerService) captureAuth(header http.Header) {
	s.managerID = header.Get(managerIDHeader)
	s.managerSecret = header.Get(managerSecretHeader)
}

func (s *managerService) observedOperations() []*codespacev1.ObservedOperation {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]*codespacev1.ObservedOperation(nil), s.observed...)
}

func (s *managerService) callState() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.sawFetch, s.sawToken
}

func (s *managerService) inventoryState() ([]int64, []*codespacev1.RuntimeInstanceRef) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]int64(nil), s.inventoryGen...), append([]*codespacev1.RuntimeInstanceRef(nil), s.inventory...)
}

func (s *managerService) runtimeTransitions() []*codespacev1.ReportRuntimeTransitionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]*codespacev1.ReportRuntimeTransitionRequest(nil), s.transitions...)
}

func (s *managerService) idleStopRequests() []*codespacev1.RequestIdleStopRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]*codespacev1.RequestIdleStopRequest(nil), s.idleStop...)
}

func (s *managerService) onlineBeforeInventory() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.onlineBeforeInv
}

func (s *managerService) onlineWasDeclared() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.sawOnline
}

type transientFetchService struct {
	codespacev1connect.UnimplementedManagerServiceHandler

	fetches      atomic.Int64
	fetchedTwice chan struct{}
}

func (s *transientFetchService) DeclareManager(
	context.Context,
	*connect.Request[codespacev1.DeclareManagerRequest],
) (*connect.Response[codespacev1.DeclareManagerResponse], error) {
	return connect.NewResponse(validDeclareResponse()), nil
}

func (s *transientFetchService) FetchOperations(
	context.Context,
	*connect.Request[codespacev1.FetchOperationsRequest],
) (*connect.Response[codespacev1.FetchOperationsResponse], error) {
	if s.fetches.Add(1) == 1 {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("temporary control plane error"))
	}
	select {
	case <-s.fetchedTwice:
	default:
		close(s.fetchedTwice)
	}
	return connect.NewResponse(&codespacev1.FetchOperationsResponse{}), nil
}

func (s *transientFetchService) ReportInstances(
	context.Context,
	*connect.Request[codespacev1.ReportInstancesRequest],
) (*connect.Response[codespacev1.ReportInstancesResponse], error) {
	return connect.NewResponse(&codespacev1.ReportInstancesResponse{}), nil
}

type protocolMismatchService struct {
	codespacev1connect.UnimplementedManagerServiceHandler
}

func (s *protocolMismatchService) DeclareManager(
	context.Context,
	*connect.Request[codespacev1.DeclareManagerRequest],
) (*connect.Response[codespacev1.DeclareManagerResponse], error) {
	return nil, testFailureError(connect.CodeFailedPrecondition, failureProtocolMismatch)
}

func validDeclareResponse() *codespacev1.DeclareManagerResponse {
	return &codespacev1.DeclareManagerResponse{
		HeartbeatIntervalMilliseconds:              1000,
		RuntimeMetadataRefreshIntervalMilliseconds: 1000,
		ControlPlaneMaxMessageSizeBytes:            1 << 20,
		GiteaWebUrl:                                "https://gitea.example.com/",
	}
}

func testFailureError(code connect.Code, category string) error {
	connectErr := connect.NewError(code, errors.New(category))
	detail, err := connect.NewErrorDetail(&codespacev1.FailureDetail{Category: category})
	if err == nil {
		connectErr.AddDetail(detail)
	}
	return connectErr
}

type blockingProvisioner struct {
	base    *provisioner.DummyProvisioner
	once    sync.Once
	started chan struct{}
	stopped chan struct{}
	release chan struct{}
}

type credentialWriteRecord struct {
	instanceName string
	request      provisioner.CredentialRequest
}

type credentialTrackingProvisioner struct {
	base *provisioner.DummyProvisioner
	mu   sync.Mutex

	writes []credentialWriteRecord
}

type nonDeletingProvisioner struct {
	base *provisioner.DummyProvisioner
}

func newCredentialTrackingProvisioner() *credentialTrackingProvisioner {
	return &credentialTrackingProvisioner{base: provisioner.NewDummy()}
}

func newBlockingProvisioner() *blockingProvisioner {
	return &blockingProvisioner{
		base:    provisioner.NewDummy(),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (p *credentialTrackingProvisioner) CreateOrStart(ctx context.Context, spec provisioner.InstanceSpec) (*provisioner.Instance, error) {
	return p.base.CreateOrStart(ctx, spec)
}

func (p *credentialTrackingProvisioner) StartExisting(ctx context.Context, spec provisioner.InstanceSpec) (*provisioner.Instance, error) {
	return p.base.StartExisting(ctx, spec)
}

func (p *credentialTrackingProvisioner) ListInstances(ctx context.Context) ([]*provisioner.Instance, error) {
	return p.base.ListInstances(ctx)
}

func (p *credentialTrackingProvisioner) WriteCredentials(ctx context.Context, instanceName string, request provisioner.CredentialRequest) error {
	if err := p.base.WriteCredentials(ctx, instanceName, request); err != nil {
		return err
	}
	p.mu.Lock()
	p.writes = append(p.writes, credentialWriteRecord{
		instanceName: instanceName,
		request:      request,
	})
	p.mu.Unlock()
	return nil
}

func (p *credentialTrackingProvisioner) Bootstrap(ctx context.Context, instanceName string, request provisioner.BootstrapRequest) error {
	return p.base.Bootstrap(ctx, instanceName, request)
}

func (p *credentialTrackingProvisioner) Stop(ctx context.Context, instanceName string) error {
	return p.base.Stop(ctx, instanceName)
}

func (p *credentialTrackingProvisioner) Delete(ctx context.Context, instanceName string) error {
	return p.base.Delete(ctx, instanceName)
}

func (p *credentialTrackingProvisioner) credentialWrites() []credentialWriteRecord {
	p.mu.Lock()
	defer p.mu.Unlock()

	writes := make([]credentialWriteRecord, len(p.writes))
	copy(writes, p.writes)
	return writes
}

func (p *blockingProvisioner) CreateOrStart(ctx context.Context, spec provisioner.InstanceSpec) (*provisioner.Instance, error) {
	return p.base.CreateOrStart(ctx, spec)
}

func (p *blockingProvisioner) StartExisting(ctx context.Context, spec provisioner.InstanceSpec) (*provisioner.Instance, error) {
	return p.base.StartExisting(ctx, spec)
}

func (p *blockingProvisioner) ListInstances(ctx context.Context) ([]*provisioner.Instance, error) {
	return p.base.ListInstances(ctx)
}

func (p *blockingProvisioner) WriteCredentials(ctx context.Context, instanceName string, request provisioner.CredentialRequest) error {
	return p.base.WriteCredentials(ctx, instanceName, request)
}

func (p *blockingProvisioner) Bootstrap(ctx context.Context, instanceName string, request provisioner.BootstrapRequest) error {
	p.once.Do(func() {
		close(p.started)
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
	}
	return p.base.Bootstrap(ctx, instanceName, request)
}

func (p *blockingProvisioner) Stop(ctx context.Context, instanceName string) error {
	select {
	case <-p.stopped:
	default:
		close(p.stopped)
	}
	return p.base.Stop(ctx, instanceName)
}

func (p *blockingProvisioner) Delete(ctx context.Context, instanceName string) error {
	return p.base.Delete(ctx, instanceName)
}

func (p *nonDeletingProvisioner) CreateOrStart(ctx context.Context, spec provisioner.InstanceSpec) (*provisioner.Instance, error) {
	return p.base.CreateOrStart(ctx, spec)
}

func (p *nonDeletingProvisioner) StartExisting(ctx context.Context, spec provisioner.InstanceSpec) (*provisioner.Instance, error) {
	return p.base.StartExisting(ctx, spec)
}

func (p *nonDeletingProvisioner) ListInstances(ctx context.Context) ([]*provisioner.Instance, error) {
	return p.base.ListInstances(ctx)
}

func (p *nonDeletingProvisioner) WriteCredentials(ctx context.Context, instanceName string, request provisioner.CredentialRequest) error {
	return p.base.WriteCredentials(ctx, instanceName, request)
}

func (p *nonDeletingProvisioner) Bootstrap(ctx context.Context, instanceName string, request provisioner.BootstrapRequest) error {
	return p.base.Bootstrap(ctx, instanceName, request)
}

func (p *nonDeletingProvisioner) Stop(ctx context.Context, instanceName string) error {
	return p.base.Stop(ctx, instanceName)
}

func (p *nonDeletingProvisioner) Delete(ctx context.Context, _ string) error {
	return ctx.Err()
}

func waitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("operation did not start")
	}
}

func waitFinalized(t *testing.T, finalized <-chan struct{}) {
	t.Helper()
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatalf("operation was not finalized")
	}
}

func waitInventoryReported(t *testing.T, reported <-chan struct{}) {
	t.Helper()
	select {
	case <-reported:
	case <-time.After(time.Second):
		t.Fatalf("inventory was not reported")
	}
}

func waitStopped(t *testing.T, stopped <-chan struct{}) {
	t.Helper()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatalf("instance was not stopped")
	}
}

func waitDeleted(t *testing.T, store *memoryOperationStateStore, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.deletedCount() == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("deleted active operations = %d", store.deletedCount())
}

func waitSavedStage(t *testing.T, store *memoryOperationStateStore, stage OperationWorkerStage) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.savedStage() == stage {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("saved worker stage = %q", store.savedStage())
}

func waitOperationCleared(t *testing.T, agent *Agent, codespaceUUID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if agent.currentOperationVersion(codespaceUUID) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operation version after final = %d", agent.currentOperationVersion(codespaceUUID))
}

type memoryOperationStateStore struct {
	mu        sync.Mutex
	saved     int
	deleted   int
	lastStage OperationWorkerStage
}

type memoryInventoryStateStore struct {
	mu          sync.Mutex
	generations []int64
}

type memoryRuntimeStateStore struct {
	mu      sync.Mutex
	saveErr error
	saved   []RuntimeTransitionSnapshot
	cleared []int64
}

type memoryRuntimeCredentialStore struct {
	mu     sync.Mutex
	tokens []runtimeCredentialRecord
}

type memoryRuntimeMetadataStateStore struct {
	mu        sync.Mutex
	snapshots []RuntimeMetadataSnapshot
}

type memoryRuntimeMetadataPublisher struct {
	mu             sync.Mutex
	codespaceUUIDs []string
	notified       []string
	err            error
}

type memoryAccessController struct {
	mu             sync.Mutex
	codespaceUUIDs []string
}

type runtimeCredentialRecord struct {
	codespaceUUID string
	token         string
}

type memoryCleanupStateStore struct {
	mu      sync.Mutex
	saveErr error
	saved   []string
	cleared []string
}

func (s *memoryInventoryStateStore) SaveInventoryGeneration(generation int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.generations = append(s.generations, generation)
	return nil
}

func (s *memoryInventoryStateStore) savedGenerations() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]int64(nil), s.generations...)
}

func (s *memoryRuntimeStateStore) SaveRuntimeTransitionPending(snapshot RuntimeTransitionSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, snapshot)
	return nil
}

func (s *memoryRuntimeStateStore) ClearRuntimeTransitionPending(_ string, runtimeGeneration int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleared = append(s.cleared, runtimeGeneration)
	return nil
}

func (s *memoryRuntimeStateStore) state() ([]RuntimeTransitionSnapshot, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]RuntimeTransitionSnapshot(nil), s.saved...), append([]int64(nil), s.cleared...)
}

func (s *memoryRuntimeCredentialStore) SaveRuntimeCredential(codespaceUUID, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.tokens = append(s.tokens, runtimeCredentialRecord{codespaceUUID: codespaceUUID, token: token})
	return nil
}

func (s *memoryRuntimeCredentialStore) savedTokens() []runtimeCredentialRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]runtimeCredentialRecord(nil), s.tokens...)
}

func (s *memoryRuntimeMetadataStateStore) SaveRuntimeMetadataSnapshot(snapshot RuntimeMetadataSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshots = append(s.snapshots, snapshot)
	return nil
}

func (s *memoryRuntimeMetadataStateStore) savedSnapshots() []RuntimeMetadataSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]RuntimeMetadataSnapshot(nil), s.snapshots...)
}

func (p *memoryRuntimeMetadataPublisher) PublishRuntimeMetadata(_ context.Context, codespaceUUID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.err != nil {
		return p.err
	}
	p.codespaceUUIDs = append(p.codespaceUUIDs, codespaceUUID)
	return nil
}

func (p *memoryRuntimeMetadataPublisher) NotifyRuntimeMetadata(codespaceUUID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.notified = append(p.notified, codespaceUUID)
}

func (p *memoryRuntimeMetadataPublisher) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.codespaceUUIDs...)
}

func (c *memoryAccessController) CloseCodespaceAccess(codespaceUUID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.codespaceUUIDs = append(c.codespaceUUIDs, codespaceUUID)
}

func (c *memoryAccessController) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.codespaceUUIDs...)
}

func (s *memoryCleanupStateStore) SaveCleanupPending(codespaceUUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, codespaceUUID)
	return nil
}

func (s *memoryCleanupStateStore) ClearCodespaceState(codespaceUUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleared = append(s.cleared, codespaceUUID)
	return nil
}

func (s *memoryCleanupStateStore) state() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.saved...), append([]string(nil), s.cleared...)
}

func (s *memoryOperationStateStore) SaveActiveOperation(snapshot OperationSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if snapshot.Payload == nil {
		return nil
	}
	s.saved++
	s.lastStage = snapshot.WorkerStage
	return nil
}

func (s *memoryOperationStateStore) DeleteActiveOperation(_ string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.deleted++
	return nil
}

func (s *memoryOperationStateStore) savedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saved
}

func (s *memoryOperationStateStore) deletedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.deleted
}

func (s *memoryOperationStateStore) savedStage() OperationWorkerStage {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastStage
}
