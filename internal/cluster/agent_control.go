// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"sync"
	"time"

	"connectrpc.com/connect"
	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	"gitea.dev/codespace-proto-go/agent/v1/agentv1connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type executionLease struct {
	version  int64
	deadline time.Time
}

// ExecutionAuthority is rebuilt only from current Gitea replies in the leader term.
type ExecutionAuthority struct {
	mu     sync.Mutex
	leases map[types.UID]executionLease
}

func (a *ExecutionAuthority) Grant(uid types.UID, version int64, deadline time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leases == nil {
		a.leases = make(map[types.UID]executionLease)
	}
	if old := a.leases[uid]; old.version > version {
		return
	}
	a.leases[uid] = executionLease{version: version, deadline: deadline}
}

func (a *ExecutionAuthority) Remaining(uid types.UID, version int64) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	lease := a.leases[uid]
	if lease.version != version || !time.Now().Before(lease.deadline) {
		return 0
	}
	return time.Until(lease.deadline)
}

func (a *ExecutionAuthority) Revoke(uid types.UID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.leases, uid)
}

type agentPeerKey struct{}

type agentSession struct {
	cancel context.CancelFunc
}

var endpointIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,28}[a-z0-9])?$`)

type AgentControlServer struct {
	Client              client.Client
	IdentityIndex       client.Reader
	ManagementNamespace string
	Tickets             *AccessTickets
	Caches              *ComponentServer
	Authority           ExecutionAuthority
	Samples             ResourceSamples
	mu                  sync.Mutex
	sessions            map[types.UID]*agentSession
}

func (s *AgentControlServer) Handler() (string, http.Handler) {
	path, handler := agentv1connect.NewAgentControlServiceHandler(s, connect.WithReadMaxBytes(api.MaxObjectBytes), connect.WithSendMaxBytes(api.MaxObjectBytes))
	return path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "Agent client certificate is required", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), agentPeerKey{}, r.TLS.PeerCertificates[0])
		if r.URL.Path == agentv1connect.AgentControlServiceControlProcedure {
			cs, err := s.current(ctx)
			if err != nil {
				http.Error(w, "Agent identity is no longer current", http.StatusForbidden)
				return
			}
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			current := &agentSession{cancel: cancel}
			// Closing the request body also interrupts an idle stream Receive.
			stopClose := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
			s.mu.Lock()
			if s.sessions == nil {
				s.sessions = make(map[types.UID]*agentSession)
			}
			if previous := s.sessions[cs.UID]; previous != nil {
				previous.cancel()
			}
			s.sessions[cs.UID] = current
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				if s.sessions[cs.UID] == current {
					delete(s.sessions, cs.UID)
				}
				s.mu.Unlock()
				cancel()
				stopClose()
			}()
		}
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *AgentControlServer) current(ctx context.Context) (*api.Codespace, error) {
	certificate, _ := ctx.Value(agentPeerKey{}).(*x509.Certificate)
	cs, _, err := ResolveAgent(ctx, s.IdentityIndex, s.Client, certificate)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}
	return cs, nil
}

func (s *AgentControlServer) gitea(ctx context.Context, cs *api.Codespace) (codespacev1connect.ManagerServiceClient, error) {
	var site api.GiteaSite
	if err := s.Client.Get(ctx, types.NamespacedName{Name: cs.Spec.Site.Name}, &site); err != nil {
		return nil, err
	}
	if site.UID != cs.Spec.Site.UID || site.Status.CanonicalURL == "" {
		return nil, fmt.Errorf("site identity is not verified")
	}
	return siteManagerClient(ctx, s.Client, s.ManagementNamespace, &site)
}

func (s *AgentControlServer) Control(ctx context.Context, stream *connect.BidiStream[agentv1.ControlRequest, agentv1.ControlResponse]) error {
	if _, err := s.current(ctx); err != nil {
		return err
	}
	var session string
	var sequence uint64
	var accessVersion int64
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if request.ProtocolVersion != 1 || request.SessionId == "" || len(request.SessionId) > 64 || (session != "" && request.SessionId != session) || request.Sequence <= sequence || request.Report == nil || request.AcceptedOperationRversion < 0 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid Agent control session"))
		}
		session, sequence = request.SessionId, request.Sequence
		cs, err := s.current(ctx)
		if err != nil {
			return err
		}
		remaining := s.Authority.Remaining(cs.UID, cs.Spec.Operation.Version)
		response := &agentv1.ControlResponse{SessionId: session, Sequence: sequence, OperationRversion: cs.Spec.Operation.Version, AccessVerificationKey: s.Tickets.PublicKey(), CancelExecution: remaining < time.Millisecond || cs.Status.SettledOperationVersion >= cs.Spec.Operation.Version}
		if request.AcceptedOperationRversion > cs.Spec.Operation.Version {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("agent accepted operation history is ahead of Gitea"))
		}
		if request.Report.OperationRversion > cs.Spec.Operation.Version {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("agent operation history is ahead of Gitea"))
		}
		currentReport := request.Report.OperationRversion == cs.Spec.Operation.Version
		// Reporting a completed fact does not grant permission to execute more work.
		if cs.Spec.Operation.Type == "abort_create" || cs.Spec.Operation.Type == "abort_resume" || cs.Status.RecoveryAction != "" {
			response.CancelExecution = true
		} else if currentReport {
			result := request.Report.Result
			if err := s.saveReport(ctx, cs, request.Report); err != nil {
				return err
			}
			s.Samples.Record(cs, request.Report.ResourceUsage)
			if result != nil {
				response.AcknowledgedResultVersion = result.OperationRversion
			}
		} else if request.Report.OperationRversion == 0 && (request.Report.Result != nil || request.Report.Boot != nil || request.Report.Target != nil) {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent facts require an operation version"))
		}
		if cs.Status.Result != nil && cs.Status.Result.Version == cs.Spec.Operation.Version {
			response.CancelExecution = true
		}
		if response.CancelExecution {
			if err := stream.Send(response); err != nil {
				return err
			}
			continue
		}
		operation := &codespacev1.OperationPayload{}
		if err := protojson.Unmarshal(cs.Spec.Operation.Payload.Raw, operation); err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("invalid persisted operation"))
		}
		if operation.OperationRversion != cs.Spec.Operation.Version || operation.RuntimeUuid != cs.Spec.RuntimeUUID || operation.CodespaceId != cs.Spec.CodespaceID {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("persisted operation identity does not match Codespace"))
		}
		if request.AcceptedOperationRversion != cs.Spec.Operation.Version {
			response.Operation = operation
			response.Runtime = &agentv1.RuntimeOptions{GitSshKeyType: cs.Spec.Runtime.GitSSHKeyType}
			if s.Caches != nil && operation.GetCreate() != nil {
				response.Runtime.Cache, err = s.Caches.runtimeCache(ctx, cs, operation)
				if err != nil {
					return connect.NewError(connect.CodeUnavailable, fmt.Errorf("prepare Runtime cache access: %w", err))
				}
			}
		}
		if (!currentReport || request.Report.Result == nil) && accessVersion != cs.Spec.Operation.Version && (cs.Spec.Operation.Type == "create" || cs.Spec.Operation.Type == "resume") {
			if len(request.Report.GitSshPublicKey) == 0 {
				// The first response supplies key options. Execution starts only after
				// the Agent has durably created its key and obtained runtime access.
				response.CancelExecution = true
				if err := stream.Send(response); err != nil {
					return err
				}
				continue
			}
			remote, err := s.gitea(ctx, cs)
			if err != nil {
				return err
			}
			access := connect.NewRequest(&codespacev1.RequestRuntimeAccessRequest{ProtocolVersion: 1, RuntimeUuid: cs.Spec.RuntimeUUID, OperationRversion: cs.Spec.Operation.Version, GitSshKey: &codespacev1.RuntimeGitSSHKey{PublicKey: request.Report.GitSshPublicKey}})
			reply, err := remote.RequestRuntimeAccess(ctx, access)
			if err != nil {
				return err
			}
			response.Access, accessVersion = reply.Msg.Access, cs.Spec.Operation.Version
		}
		response.PermitValidForMilliseconds = s.Authority.Remaining(cs.UID, cs.Spec.Operation.Version).Milliseconds()
		response.CancelExecution = response.PermitValidForMilliseconds <= 0
		if err := stream.Send(response); err != nil {
			return err
		}
	}
}

func (s *AgentControlServer) saveReport(ctx context.Context, cs *api.Codespace, report *agentv1.AgentReport) error {
	if report == nil || report.OperationRversion != cs.Spec.Operation.Version {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("agent report operation changed"))
	}
	before := cs.DeepCopy().Status
	result := report.Result
	if result != nil && (result.OperationRversion != cs.Spec.Operation.Version || len(result.Message) > 4096 || (result.Status != codespacev1.FinalStatus_FINAL_STATUS_DONE && result.Status != codespacev1.FinalStatus_FINAL_STATUS_FAILED)) {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid Agent operation result"))
	}
	startup := cs.Spec.Operation.Type == "create" || cs.Spec.Operation.Type == "resume"
	if result != nil && result.Status == codespacev1.FinalStatus_FINAL_STATUS_DONE && startup {
		if report.Target == nil || !report.Target.Ready || report.Target.Version <= 0 || report.Target.PrimaryContainerId == "" || report.Boot == nil || report.Boot.OperationRversion != cs.Spec.Operation.Version || report.Boot.Stage != codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("successful startup requires a ready access target"))
		}
	}
	previousBoot := cs.Status.Boot
	previousTarget := cs.Status.Target
	if report.Boot != nil {
		if !startup || report.Boot.OperationRversion != cs.Spec.Operation.Version || report.Boot.Stage < codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_RUNTIME || report.Boot.Stage > codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY || report.Boot.StartedUnix <= 0 || report.Boot.LastUpdateUnix < report.Boot.StartedUnix {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid Agent boot report"))
		}
		if previous, err := runtimeBootToProto(cs.Status.Boot); err != nil {
			return connect.NewError(connect.CodeInternal, err)
		} else if previous != nil && previous.OperationRversion == report.Boot.OperationRversion && (previous.Stage > report.Boot.Stage || previous.LastUpdateUnix > report.Boot.LastUpdateUnix || previous.StartedUnix != report.Boot.StartedUnix) {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("agent boot progress regressed"))
		}
		boot, err := runtimeBootFromProto(report.Boot)
		if err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
		cs.Status.Boot = boot
	}
	if report.Target != nil {
		if !startup || report.Target.Version <= 0 || (report.Target.Ready && report.Target.PrimaryContainerId == "") {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid Agent access target"))
		}
		target := &api.AgentTarget{Version: report.Target.Version, Ready: report.Target.Ready, PrimaryContainerID: report.Target.PrimaryContainerId}
		if len(report.Target.Endpoints) > 64 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("too many endpoints"))
		}
		seen := make(map[string]bool)
		for _, endpoint := range report.Target.Endpoints {
			if endpoint == nil || endpoint.Endpoint == nil || !endpointIDPattern.MatchString(endpoint.Endpoint.EndpointId) || endpoint.ContainerId == "" || endpoint.Port == 0 || endpoint.Port > 65535 || seen[endpoint.Endpoint.EndpointId] {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid endpoint target"))
			}
			if endpoint.Endpoint.EndpointId == runtimeendpoint.WorkspaceEndpointID {
				if endpoint.Endpoint.Public || endpoint.Endpoint.Port != 0 || endpoint.ContainerId != target.PrimaryContainerID {
					return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("workspace must be private, omit its display port, and belong to the primary container"))
				}
			} else if endpoint.Endpoint.Port != endpoint.Port {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("endpoint display port does not match its target"))
			}
			if err := runtimeendpoint.ValidateLabel(endpoint.Endpoint.Label); err != nil {
				return connect.NewError(connect.CodeInvalidArgument, err)
			}
			seen[endpoint.Endpoint.EndpointId] = true
			target.Endpoints = append(target.Endpoints, api.AgentEndpoint{ID: endpoint.Endpoint.EndpointId, Label: endpoint.Endpoint.Label, Public: endpoint.Endpoint.Public, ContainerID: endpoint.ContainerId, Port: int32(endpoint.Port)})
		}
		if target.Ready && !seen[runtimeendpoint.WorkspaceEndpointID] {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("ready target requires a workspace endpoint"))
		}
		if previous := cs.Status.Target; previous != nil && (target.Version < previous.Version || (target.Version == previous.Version && !reflect.DeepEqual(target, previous))) {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("access target version does not identify the reported content"))
		}
		cs.Status.Target = target
	}
	if !reflect.DeepEqual(previousBoot, cs.Status.Boot) || !reflect.DeepEqual(previousTarget, cs.Status.Target) {
		if cs.Status.MetadataGeneration == int64(^uint64(0)>>1) {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("runtime metadata generation exhausted"))
		}
		cs.Status.MetadataGeneration++
	}
	if result != nil {
		stored := &api.OperationResult{Version: result.OperationRversion, Succeeded: result.Status == codespacev1.FinalStatus_FINAL_STATUS_DONE, Message: result.Message}
		if previous := cs.Status.Result; previous != nil && previous.Version == stored.Version {
			if *previous != *stored {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("operation result was already recorded with different content"))
			}
		}
		cs.Status.Result = stored
	}
	if err := api.ValidateObjectSize(cs); err != nil {
		return connect.NewError(connect.CodeResourceExhausted, err)
	}
	if equality.Semantic.DeepEqual(before, cs.Status) {
		return nil
	}
	return s.Client.Status().Update(ctx, cs)
}

func (s *AgentControlServer) UploadLogs(ctx context.Context, request *connect.Request[agentv1.UploadLogsRequest]) (*connect.Response[agentv1.UploadLogsResponse], error) {
	cs, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	message := request.Msg
	if message.ProtocolVersion != 1 || message.OperationRversion != cs.Spec.Operation.Version || message.Offset < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid Agent log operation or offset"))
	}
	remote, err := s.gitea(ctx, cs)
	if err != nil {
		return nil, err
	}
	forward := connect.NewRequest(&codespacev1.UpdateLogRequest{ProtocolVersion: 1, RuntimeUuid: cs.Spec.RuntimeUUID, OperationRversion: message.OperationRversion, Offset: message.Offset, Lines: message.Lines})
	response, err := remote.UpdateLog(ctx, forward)
	if err != nil {
		var remoteError *connect.Error
		if errors.As(err, &remoteError) {
			for _, detail := range remoteError.Details() {
				value, detailErr := detail.Value()
				if detailErr != nil {
					continue
				}
				failure, ok := value.(*codespacev1.FailureDetail)
				if !ok {
					continue
				}
				switch failure.Category {
				case "log_size_exceeded", "stale_operation", "codespace_not_found":
					// Closing retires this batch without claiming its bytes were appended.
					return connect.NewResponse(&agentv1.UploadLogsResponse{NextOffset: message.Offset, Closed: true}), nil
				}
			}
		}
		return nil, err
	}
	return connect.NewResponse(&agentv1.UploadLogsResponse{NextOffset: response.Msg.NextOffset}), nil
}
