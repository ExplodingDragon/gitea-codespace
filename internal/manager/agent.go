// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/provisioner"
	"gitea.dev/codespace/internal/store"
)

const (
	managerUUIDHeader  = "x-codespace-manager-uuid"
	managerTokenHeader = "x-codespace-manager-token"
)

// AgentConfig configures the embedded manager worker.
type AgentConfig struct {
	BaseURL       string
	ManagerUUID   string
	ManagerToken  string
	Name          string
	GatewayURL    string
	Version       string
	PollInterval  time.Duration
	PingInterval  time.Duration
	FetchCapacity int32
	RuntimeAPIURL string
	Capabilities  *codespacev1.ManagerCapabilities
}

// Agent runs one embedded manager against the control plane.
type Agent struct {
	config      AgentConfig
	client      codespacev1connect.CodespaceServiceClient
	httpClient  *http.Client
	provisioner provisioner.Provisioner
	store       *store.MemoryStore
}

// New creates one embedded manager.
func New(
	config AgentConfig,
	httpClient *http.Client,
	provisioner provisioner.Provisioner,
	memoryStore *store.MemoryStore,
) *Agent {
	client := codespacev1connect.NewCodespaceServiceClient(httpClient, config.BaseURL)
	return &Agent{
		config:      config,
		client:      client,
		httpClient:  httpClient,
		provisioner: provisioner,
		store:       memoryStore,
	}
}

// Run starts the manager loops and blocks until the context is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	pingTicker := time.NewTicker(a.config.PingInterval)
	defer pingTicker.Stop()

	pollTicker := time.NewTicker(a.config.PollInterval)
	defer pollTicker.Stop()

	if err := a.declare(ctx); err != nil {
		return fmt.Errorf("declare manager: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pingTicker.C:
			if err := a.ping(ctx); err != nil {
				return fmt.Errorf("manager ping: %w", err)
			}
		case <-pollTicker.C:
			if err := a.pollOnce(ctx); err != nil {
				return fmt.Errorf("poll tasks: %w", err)
			}
		}
	}
}

func (a *Agent) ping(ctx context.Context) error {
	request := connect.NewRequest(&codespacev1.PingRequest{
		Status:             codespacev1.ManagerStatus_MANAGER_STATUS_ONLINE,
		CurrentConcurrency: 0,
		Load:               0,
	})
	a.setManagerAuth(request.Header())
	if _, err := a.client.Ping(ctx, request); err != nil {
		return fmt.Errorf("ping rpc: %w", err)
	}
	return nil
}

func (a *Agent) declare(ctx context.Context) error {
	request := connect.NewRequest(&codespacev1.DeclareManagerRequest{
		Name:         a.config.Name,
		GatewayUrl:   a.config.GatewayURL,
		Version:      a.config.Version,
		Capabilities: a.capabilities(),
	})
	a.setManagerAuth(request.Header())
	if _, err := a.client.DeclareManager(ctx, request); err != nil {
		return fmt.Errorf("declare manager rpc: %w", err)
	}
	return nil
}

func (a *Agent) pollOnce(ctx context.Context) error {
	request := connect.NewRequest(&codespacev1.FetchTaskRequest{
		Capacity: a.config.FetchCapacity,
	})
	a.setManagerAuth(request.Header())
	response, err := a.client.FetchTask(ctx, request)
	if err != nil {
		return fmt.Errorf("fetch tasks rpc: %w", err)
	}
	for _, operation := range response.Msg.GetTasks() {
		if err := a.handleOperation(ctx, operation); err != nil {
			return fmt.Errorf("handle operation %s: %w", operation.GetOperationId(), err)
		}
	}
	return nil
}

func (a *Agent) handleOperation(ctx context.Context, operation *codespacev1.CodespaceOperation) error {
	if err := a.appendLog(ctx, operation, "info", "operation started"); err != nil {
		return err
	}
	switch operation.GetType() {
	case codespacev1.OperationType_OPERATION_TYPE_CREATE, codespacev1.OperationType_OPERATION_TYPE_RESUME:
		return a.handleCreateOrResume(ctx, operation)
	case codespacev1.OperationType_OPERATION_TYPE_STOP:
		return a.handleStop(ctx, operation)
	case codespacev1.OperationType_OPERATION_TYPE_DELETE:
		return a.handleDelete(ctx, operation)
	default:
		return a.finishTask(ctx, operation, codespacev1.OperationStatus_OPERATION_STATUS_CANCELLED, nil, "unsupported operation type")
	}
}

func (a *Agent) handleCreateOrResume(ctx context.Context, operation *codespacev1.CodespaceOperation) error {
	spec := provisioner.InstanceSpec{
		Name:         operation.GetPayload().GetInstanceName(),
		Type:         operation.GetPayload().GetInstanceType(),
		Image:        operation.GetPayload().GetImage(),
		RepoFullName: operation.GetPayload().GetRepoFullName(),
	}
	var (
		instance *provisioner.Instance
		err      error
	)
	if operation.GetType() == codespacev1.OperationType_OPERATION_TYPE_CREATE {
		instance, err = a.provisioner.CreateOrStart(spec)
	} else {
		instance, err = a.provisioner.StartExisting(spec)
	}
	if err != nil {
		_ = a.appendLog(ctx, operation, "error", err.Error())
		return a.finishTask(ctx, operation, codespacev1.OperationStatus_OPERATION_STATUS_FAILED, nil, err.Error())
	}
	if err := a.appendLog(ctx, operation, "info", "instance is running"); err != nil {
		return err
	}
	requestGitToken := connect.NewRequest(&codespacev1.RequestGitTokenRequest{
		CodespaceId: operation.GetCodespaceId(),
		OperationId: operation.GetOperationId(),
	})
	a.setManagerAuth(requestGitToken.Header())
	gitTokenResponse, err := a.client.RequestGitToken(ctx, requestGitToken)
	if err != nil {
		return fmt.Errorf("request git token: %w", err)
	}

	runtimeContext, err := a.store.PrepareRuntime(
		operation.GetCodespaceId(),
		operation.GetPayload().GetRepoFullName(),
		operation.GetPayload().GetStartRef(),
		instance.Workdir,
	)
	if err != nil {
		return fmt.Errorf("prepare runtime: %w", err)
	}
	if _, err := a.store.UpdateRuntimeStatus(runtimeContext.Token, "cloning", "cloning repository"); err != nil {
		return fmt.Errorf("mark runtime cloning: %w", err)
	}
	if err := a.provisioner.Bootstrap(instance.Name, provisioner.BootstrapRequest{
		CodespaceID:   operation.GetCodespaceId(),
		RuntimeToken:  runtimeContext.Token,
		RuntimeAPIURL: a.config.RuntimeAPIURL,
		RepoURL:       operation.GetPayload().GetRepoUrl(),
		RepoFullName:  operation.GetPayload().GetRepoFullName(),
		StartRef:      operation.GetPayload().GetStartRef(),
		StartSHA:      operation.GetPayload().GetStartSha(),
		Workdir:       instance.Workdir,
		InitScript:    operation.GetPayload().GetInitScript(),
		GitUsername:   gitTokenResponse.Msg.GetUsername(),
		GitToken:      gitTokenResponse.Msg.GetToken(),
	}); err != nil {
		_, _ = a.store.UpdateRuntimeStatus(runtimeContext.Token, "failed", err.Error())
		_ = a.appendLog(ctx, operation, "error", err.Error())
		return a.finishTask(ctx, operation, codespacev1.OperationStatus_OPERATION_STATUS_FAILED, nil, err.Error())
	}
	if _, err := a.store.UpdateRuntimeStatus(runtimeContext.Token, "ready", "codespace ready"); err != nil {
		return fmt.Errorf("mark runtime ready: %w", err)
	}
	if err := a.reportStatus(ctx, operation, codespacev1.CodespaceStatus_CODESPACE_STATUS_RUNNING, ""); err != nil {
		return err
	}
	return a.finishTask(ctx, operation, codespacev1.OperationStatus_OPERATION_STATUS_SUCCEEDED, &codespacev1.TaskResult{
		InstanceName: instance.Name,
		Workdir:      instance.Workdir,
	}, "")
}

func (a *Agent) handleStop(ctx context.Context, operation *codespacev1.CodespaceOperation) error {
	if err := a.provisioner.Stop(operation.GetPayload().GetInstanceName()); err != nil {
		_ = a.appendLog(ctx, operation, "error", err.Error())
		return a.finishTask(ctx, operation, codespacev1.OperationStatus_OPERATION_STATUS_FAILED, nil, err.Error())
	}
	revokeRequest := connect.NewRequest(&codespacev1.RevokeGitTokenRequest{
		CodespaceId: operation.GetCodespaceId(),
		Reason:      "codespace stopped",
		OperationId: operation.GetOperationId(),
	})
	a.setManagerAuth(revokeRequest.Header())
	if _, err := a.client.RevokeGitToken(ctx, revokeRequest); err != nil {
		return fmt.Errorf("revoke git token: %w", err)
	}
	a.store.ClearRuntime(operation.GetCodespaceId())
	if err := a.reportStatus(ctx, operation, codespacev1.CodespaceStatus_CODESPACE_STATUS_STOPPED, ""); err != nil {
		return err
	}
	return a.finishTask(ctx, operation, codespacev1.OperationStatus_OPERATION_STATUS_SUCCEEDED, nil, "")
}

func (a *Agent) handleDelete(ctx context.Context, operation *codespacev1.CodespaceOperation) error {
	if err := a.provisioner.Delete(operation.GetPayload().GetInstanceName()); err != nil {
		_ = a.appendLog(ctx, operation, "error", err.Error())
		return a.finishTask(ctx, operation, codespacev1.OperationStatus_OPERATION_STATUS_FAILED, nil, err.Error())
	}
	revokeRequest := connect.NewRequest(&codespacev1.RevokeGitTokenRequest{
		CodespaceId: operation.GetCodespaceId(),
		Reason:      "codespace deleted",
		OperationId: operation.GetOperationId(),
	})
	a.setManagerAuth(revokeRequest.Header())
	if _, err := a.client.RevokeGitToken(ctx, revokeRequest); err != nil {
		return fmt.Errorf("revoke git token: %w", err)
	}
	a.store.ClearRuntime(operation.GetCodespaceId())
	if err := a.reportStatus(ctx, operation, codespacev1.CodespaceStatus_CODESPACE_STATUS_DELETING, ""); err != nil {
		return err
	}
	return a.finishTask(ctx, operation, codespacev1.OperationStatus_OPERATION_STATUS_SUCCEEDED, nil, "")
}

func (a *Agent) finishTask(
	ctx context.Context,
	operation *codespacev1.CodespaceOperation,
	status codespacev1.OperationStatus,
	result *codespacev1.TaskResult,
	errorMessage string,
) error {
	request := connect.NewRequest(&codespacev1.FinishTaskRequest{
		TaskId:      operation.GetTaskId(),
		Status:      status,
		Result:      result,
		Error:       errorMessage,
		OperationId: operation.GetOperationId(),
	})
	a.setManagerAuth(request.Header())
	if _, err := a.client.FinishTask(ctx, request); err != nil {
		return fmt.Errorf("finish task rpc: %w", err)
	}
	return nil
}

func (a *Agent) reportStatus(
	ctx context.Context,
	operation *codespacev1.CodespaceOperation,
	status codespacev1.CodespaceStatus,
	errorMessage string,
) error {
	request := connect.NewRequest(&codespacev1.ReportCodespaceStatusRequest{
		CodespaceId:    operation.GetCodespaceId(),
		Status:         status,
		LastActiveUnix: time.Now().Unix(),
		Error:          errorMessage,
		OperationId:    operation.GetOperationId(),
	})
	a.setManagerAuth(request.Header())
	if _, err := a.client.ReportCodespaceStatus(ctx, request); err != nil {
		return fmt.Errorf("report codespace status rpc: %w", err)
	}
	return nil
}

func (a *Agent) appendLog(ctx context.Context, operation *codespacev1.CodespaceOperation, level, message string) error {
	request := connect.NewRequest(&codespacev1.AppendLogRequest{
		CodespaceId: operation.GetCodespaceId(),
		OperationId: operation.GetOperationId(),
		TaskId:      operation.GetTaskId(),
		Level:       level,
		Message:     message,
	})
	a.setManagerAuth(request.Header())
	if _, err := a.client.AppendLog(ctx, request); err != nil {
		return fmt.Errorf("append log rpc: %w", err)
	}
	return nil
}

func (a *Agent) setManagerAuth(header http.Header) {
	header.Set(managerUUIDHeader, a.config.ManagerUUID)
	header.Set(managerTokenHeader, a.config.ManagerToken)
}

func (a *Agent) capabilities() *codespacev1.ManagerCapabilities {
	if a.config.Capabilities != nil {
		return a.config.Capabilities
	}
	return &codespacev1.ManagerCapabilities{
		GatewayUrl: a.config.GatewayURL,
		Version:    a.config.Version,
	}
}
