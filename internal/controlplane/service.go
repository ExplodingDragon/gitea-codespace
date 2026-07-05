// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package controlplane

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/store"
)

const (
	managerUUIDHeader  = "x-codespace-manager-uuid"
	managerTokenHeader = "x-codespace-manager-token"
)

// Service implements the codespace control-plane gRPC contract.
type Service struct {
	store *store.MemoryStore
}

var _ codespacev1connect.CodespaceServiceHandler = (*Service)(nil)

// New creates one control-plane service implementation.
func New(memoryStore *store.MemoryStore) *Service {
	return &Service{store: memoryStore}
}

// RegisterManager registers one manager.
func (s *Service) RegisterManager(
	_ context.Context,
	req *connect.Request[codespacev1.RegisterManagerRequest],
) (*connect.Response[codespacev1.RegisterManagerResponse], error) {
	manager, token, err := s.store.RegisterManager(
		req.Msg.GetRegistrationToken(),
		req.Msg.GetName(),
		req.Msg.GetGatewayUrl(),
		req.Msg.GetVersion(),
		req.Msg.GetCapabilities(),
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("register manager: %w", err))
	}
	return connect.NewResponse(&codespacev1.RegisterManagerResponse{
		ManagerId:    manager.ID,
		ManagerUuid:  manager.UUID,
		ManagerToken: token,
	}), nil
}

// DeclareManager stores manager declaration.
func (s *Service) DeclareManager(
	_ context.Context,
	req *connect.Request[codespacev1.DeclareManagerRequest],
) (*connect.Response[codespacev1.DeclareManagerResponse], error) {
	manager, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	if err := s.store.DeclareManager(
		manager.UUID,
		req.Msg.GetName(),
		req.Msg.GetGatewayUrl(),
		req.Msg.GetVersion(),
		req.Msg.GetCapabilities(),
	); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("declare manager: %w", err))
	}
	return connect.NewResponse(&codespacev1.DeclareManagerResponse{Accepted: true}), nil
}

// Ping stores one manager heartbeat.
func (s *Service) Ping(
	_ context.Context,
	req *connect.Request[codespacev1.PingRequest],
) (*connect.Response[codespacev1.PingResponse], error) {
	manager, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	if err := s.store.UpdateManagerHeartbeat(
		manager.UUID,
		req.Msg.GetStatus(),
		req.Msg.GetCurrentConcurrency(),
		req.Msg.GetLoad(),
	); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("update heartbeat: %w", err))
	}
	return connect.NewResponse(&codespacev1.PingResponse{ServerTime: time.Now().Unix()}), nil
}

// FetchTask leases tasks for one manager.
func (s *Service) FetchTask(
	_ context.Context,
	req *connect.Request[codespacev1.FetchTaskRequest],
) (*connect.Response[codespacev1.FetchTaskResponse], error) {
	manager, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	tasks := s.store.FetchTasks(manager.UUID, req.Msg.GetCapacity())
	response := &codespacev1.FetchTaskResponse{
		Tasks: make([]*codespacev1.CodespaceOperation, 0, len(tasks)),
	}
	for _, task := range tasks {
		leaseDeadline := int64(0)
		if task.LeaseDeadline != nil {
			leaseDeadline = task.LeaseDeadline.Unix()
		}
		response.Tasks = append(response.Tasks, &codespacev1.CodespaceOperation{
			TaskId:        task.ID,
			CodespaceId:   task.CodespaceID,
			Type:          task.Type,
			Payload:       task.Payload,
			LeaseDeadline: leaseDeadline,
			OperationId:   task.OperationID,
		})
	}
	return connect.NewResponse(response), nil
}

// AppendLog stores one manager log line.
func (s *Service) AppendLog(
	_ context.Context,
	req *connect.Request[codespacev1.AppendLogRequest],
) (*connect.Response[codespacev1.AppendLogResponse], error) {
	manager, err := s.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	sequence, err := s.store.AppendLog(
		req.Msg.GetCodespaceId(),
		req.Msg.GetOperationId(),
		req.Msg.GetTaskId(),
		manager.ID,
		req.Msg.GetLevel(),
		req.Msg.GetMessage(),
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("append log: %w", err))
	}
	return connect.NewResponse(&codespacev1.AppendLogResponse{Sequence: sequence}), nil
}

// FinishTask stores one task result.
func (s *Service) FinishTask(
	_ context.Context,
	req *connect.Request[codespacev1.FinishTaskRequest],
) (*connect.Response[codespacev1.FinishTaskResponse], error) {
	if _, err := s.authenticate(req.Header()); err != nil {
		return nil, err
	}
	if err := s.store.FinishTask(
		req.Msg.GetTaskId(),
		req.Msg.GetOperationId(),
		req.Msg.GetStatus(),
		req.Msg.GetResult(),
		req.Msg.GetError(),
	); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("finish task: %w", err))
	}
	return connect.NewResponse(&codespacev1.FinishTaskResponse{}), nil
}

// ReportCodespaceStatus stores runtime status updates.
func (s *Service) ReportCodespaceStatus(
	_ context.Context,
	req *connect.Request[codespacev1.ReportCodespaceStatusRequest],
) (*connect.Response[codespacev1.ReportCodespaceStatusResponse], error) {
	if _, err := s.authenticate(req.Header()); err != nil {
		return nil, err
	}
	if err := s.store.ReportCodespaceStatus(
		req.Msg.GetCodespaceId(),
		req.Msg.GetStatus(),
		req.Msg.GetLastActiveUnix(),
		req.Msg.GetError(),
		req.Msg.GetOperationId(),
	); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("report codespace status: %w", err))
	}
	return connect.NewResponse(&codespacev1.ReportCodespaceStatusResponse{}), nil
}

// ReportCodespacePorts stores runtime port updates.
func (s *Service) ReportCodespacePorts(
	_ context.Context,
	req *connect.Request[codespacev1.ReportCodespacePortsRequest],
) (*connect.Response[codespacev1.ReportCodespacePortsResponse], error) {
	if _, err := s.authenticate(req.Header()); err != nil {
		return nil, err
	}
	if err := s.store.ReportCodespacePorts(req.Msg.GetCodespaceId(), req.Msg.GetPorts()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("report codespace ports: %w", err))
	}
	return connect.NewResponse(&codespacev1.ReportCodespacePortsResponse{}), nil
}

// RequestGitToken creates one codespace git token.
func (s *Service) RequestGitToken(
	_ context.Context,
	req *connect.Request[codespacev1.RequestGitTokenRequest],
) (*connect.Response[codespacev1.RequestGitTokenResponse], error) {
	if _, err := s.authenticate(req.Header()); err != nil {
		return nil, err
	}
	username, token, expiresAt, err := s.store.IssueGitToken(
		req.Msg.GetCodespaceId(),
		req.Msg.GetOperationId(),
	)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("request git token: %w", err))
	}
	return connect.NewResponse(&codespacev1.RequestGitTokenResponse{
		Username:  username,
		Token:     token,
		ExpiresAt: expiresAt.Unix(),
	}), nil
}

// RevokeGitToken revokes one git token.
func (s *Service) RevokeGitToken(
	_ context.Context,
	req *connect.Request[codespacev1.RevokeGitTokenRequest],
) (*connect.Response[codespacev1.RevokeGitTokenResponse], error) {
	if _, err := s.authenticate(req.Header()); err != nil {
		return nil, err
	}
	if err := s.store.RevokeGitToken(req.Msg.GetCodespaceId()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("revoke git token: %w", err))
	}
	return connect.NewResponse(&codespacev1.RevokeGitTokenResponse{}), nil
}

// ValidateAccessTicket validates one gateway ticket.
func (s *Service) ValidateAccessTicket(
	_ context.Context,
	req *connect.Request[codespacev1.ValidateAccessTicketRequest],
) (*connect.Response[codespacev1.ValidateAccessTicketResponse], error) {
	record, err := s.store.ValidateAccessTicket(req.Msg.GetTicket(), req.Msg.GetAction())
	if err != nil {
		return connect.NewResponse(&codespacev1.ValidateAccessTicketResponse{Valid: false}), nil
	}
	return connect.NewResponse(&codespacev1.ValidateAccessTicketResponse{
		Valid:       true,
		UserId:      record.UserID,
		CodespaceId: record.CodespaceID,
		RepoId:      record.RepoID,
		Action:      record.Action,
		ExpiresAt:   record.ExpiresAt.Unix(),
	}), nil
}

func (s *Service) authenticate(header map[string][]string) (*store.Manager, error) {
	managerUUID := firstHeaderValue(header, managerUUIDHeader)
	managerToken := firstHeaderValue(header, managerTokenHeader)
	if managerUUID == "" || managerToken == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("missing manager auth headers: %w", store.ErrUnauthorized))
	}
	manager, err := s.store.AuthenticateManager(managerUUID, managerToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("authenticate manager: %w", err))
	}
	return manager, nil
}

func firstHeaderValue(header map[string][]string, key string) string {
	for currentKey, values := range header {
		if strings.EqualFold(currentKey, key) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
