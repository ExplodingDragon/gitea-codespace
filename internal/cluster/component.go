// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"gitea.dev/codespace/internal/rpc/component/v1/componentv1connect"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const RuntimeUUIDIndex = "codespace.gitea.dev/runtime-uuid"

type componentIdentity struct {
	Role, UID, Instance string
}

type componentIdentityContextKey struct{}

func componentIdentityHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.PeerCertificates[0].URIs) != 1 {
			http.Error(w, "component identity is required", http.StatusUnauthorized)
			return
		}
		uri := r.TLS.PeerCertificates[0].URIs[0]
		parts := strings.Split(strings.TrimPrefix(uri.Path, "/"), "/")
		if uri.Scheme != "spiffe" || uri.Host != "codespace" || len(parts) != 3 || (parts[0] != "gateway" && parts[0] != "cache") || parts[1] == "" || parts[2] == "" {
			http.Error(w, "component identity is invalid", http.StatusUnauthorized)
			return
		}
		identity := componentIdentity{Role: parts[0], UID: parts[1], Instance: parts[2]}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), componentIdentityContextKey{}, identity)))
	})
}

// ComponentServer projects Kubernetes state and Gitea authorization to a
// narrowly scoped Gateway identity. It never exposes Gitea credentials.
type ComponentServer struct {
	componentv1connect.UnimplementedComponentServiceHandler
	Client              client.Client
	IdentityIndex       client.Reader
	ManagementNamespace string
	Tickets             *AccessTickets
	Activity            *RuntimeActivityTracker
	GatewayChanges      *ChangeNotifier
	CacheChanges        *ChangeNotifier
}

func (s *ComponentServer) Handler() (string, http.Handler) {
	rpcServer := connect.NewServer()
	componentv1connect.RegisterComponentServiceHandler(rpcServer, s)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, rpcServer, connecthttp.WithReadMaxBytes(api.MaxObjectBytes), connecthttp.WithSendMaxBytes(api.MaxObjectBytes))
	return "/" + componentv1connect.ComponentServiceName + "/", mux
}

func componentIdentityFromContext(ctx context.Context, role string) (componentIdentity, error) {
	identity, ok := ctx.Value(componentIdentityContextKey{}).(componentIdentity)
	if !ok || identity.Role != role || identity.UID == "" || identity.Instance == "" {
		return componentIdentity{}, connect.Errorf(connect.CodePermissionDenied, "component identity is not authorized for %s", role)
	}
	return identity, nil
}

func (s *ComponentServer) gatewayConfig(ctx context.Context, identity componentIdentity) (*componentv1.GatewayConfigurationSnapshot, types.UID, error) {
	reader := s.IdentityIndex
	if reader == nil {
		reader = s.Client
	}
	var components corev1.ConfigMapList
	if err := reader.List(ctx, &components, client.InNamespace(s.ManagementNamespace), client.MatchingFields{IdentityUIDIndex: identity.UID}); err != nil {
		return nil, "", err
	}
	if len(components.Items) != 1 {
		return nil, "", connect.NewError(connect.CodePermissionDenied, "gateway component identity is no longer configured")
	}
	component := &components.Items[0]
	if component.Labels[ComponentLabel] != "gateway" || !component.DeletionTimestamp.IsZero() {
		return nil, "", connect.NewError(connect.CodePermissionDenied, "gateway component identity is no longer configured")
	}
	config, err := gatewayComponentConfig(component)
	if err != nil {
		return nil, "", err
	}
	return &componentv1.GatewayConfigurationSnapshot{
		HttpListen: config.HTTP.Listen, PublicUrl: config.HTTP.PublicURL, SshListen: config.SSH.Listen, SshPublicAddress: config.SSH.PublicAddr,
		SessionTtlMilliseconds: int64(config.Sessions.TTL.ToStdlib() / time.Millisecond), SessionIdleTimeoutMilliseconds: int64(config.Sessions.IdleTimeout.ToStdlib() / time.Millisecond), RevalidateIntervalMilliseconds: int64(config.Sessions.RevalidateInterval.ToStdlib() / time.Millisecond),
		MaxSessionsPerCodespace: int32(config.Sessions.MaxPerCodespace), MaxSessionsPerUser: int32(config.Sessions.MaxPerUser), MaxInflight: int32(config.Limits.MaxInflightTotal), MaxInflightPerSession: int32(config.Limits.MaxInflightPerSession), MaxChannelsPerSshConnection: int32(config.SSH.MaxChannelsPerConnection),
	}, component.UID, nil
}

func (s *ComponentServer) gatewaySnapshot(ctx context.Context, identity componentIdentity) (*componentv1.GatewayControlResponse, error) {
	configuration, componentUID, err := s.gatewayConfig(ctx, identity)
	if err != nil {
		return nil, err
	}
	if configuration.PublicUrl == "" || configuration.SshPublicAddress == "" {
		return nil, fmt.Errorf("gateway public URL and SSH address are required")
	}
	var sites api.GiteaSiteList
	if err := s.IdentityIndex.List(ctx, &sites, client.MatchingFields{SiteGatewayUIDIndex: string(componentUID)}); err != nil {
		return nil, err
	}
	response := &componentv1.GatewayControlResponse{ProtocolVersion: 1, Snapshot: true, Config: configuration}
	for i := range sites.Items {
		site := &sites.Items[i]
		if !site.Spec.Enabled || site.Spec.Gateway.UID != componentUID || !meta.IsStatusConditionTrue(site.Status.Conditions, "InfrastructureReady") {
			continue
		}
		var runtimes api.CodespaceList
		if err := s.IdentityIndex.List(ctx, &runtimes,
			client.InNamespace("codespace-"+site.Name),
			client.MatchingFields{RuntimeSiteUIDIndex: string(site.UID)},
		); err != nil {
			return nil, err
		}
		for index := range runtimes.Items {
			route, err := s.route(ctx, s.IdentityIndex, &runtimes.Items[index], componentUID)
			if err != nil {
				return nil, err
			}
			if route != nil {
				response.Runtimes = append(response.Runtimes, route)
			}
		}
	}
	slices.SortFunc(response.Runtimes, func(a, b *componentv1.GatewayRuntimeRoute) int { return strings.Compare(a.RuntimeUuid, b.RuntimeUuid) })
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(response)
	if err != nil {
		return nil, err
	}
	response.Cursor = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return response, nil
}

func (s *ComponentServer) route(ctx context.Context, reader client.Reader, cs *api.Codespace, gatewayUID types.UID) (*componentv1.GatewayRuntimeRoute, error) {
	if !cs.DeletionTimestamp.IsZero() || !cs.Status.Bound || cs.Status.RecoveryAction != "" || cs.Status.Target == nil || !cs.Status.Target.Ready || cs.Status.Pod.UID == "" {
		return nil, nil
	}
	switch cs.Spec.Operation.Type {
	case "stop", "delete", "abort_create", "abort_resume":
		return nil, nil
	}
	if result := cs.Status.Result; result != nil && result.Version == cs.Spec.Operation.Version && !result.Succeeded {
		return nil, nil
	}
	var site api.GiteaSite
	if err := reader.Get(ctx, types.NamespacedName{Name: cs.Spec.Site.Name}, &site); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if site.UID != cs.Spec.Site.UID {
		log.FromContext(ctx).Error(fmt.Errorf("runtime site identity changed"), "Reject runtime access target", "codespace", client.ObjectKeyFromObject(cs))
		return nil, nil
	}
	ready := meta.FindStatusCondition(site.Status.Conditions, "InfrastructureReady")
	if !site.Spec.Enabled || site.Spec.Gateway.UID != gatewayUID || site.Status.CanonicalURL == "" || !site.DeletionTimestamp.IsZero() || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != site.Generation {
		return nil, nil
	}
	var pod corev1.Pod
	if err := reader.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Pod.Name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if pod.UID != cs.Status.Pod.UID || !metav1.IsControlledBy(&pod, cs) {
		log.FromContext(ctx).Error(fmt.Errorf("runtime Pod identity changed"), "Reject runtime access target", "codespace", client.ObjectKeyFromObject(cs), "pod", pod.Name)
		return nil, nil
	}
	if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning {
		return nil, nil
	}
	var address string
	for _, item := range pod.Status.PodIPs {
		if ip := net.ParseIP(item.IP); ip != nil {
			address = net.JoinHostPort(item.IP, "8444")
			break
		}
	}
	if address == "" && net.ParseIP(pod.Status.PodIP) != nil {
		address = net.JoinHostPort(pod.Status.PodIP, "8444")
	}
	if address == "" {
		return nil, nil
	}
	route := &componentv1.GatewayRuntimeRoute{SiteUid: string(cs.Spec.Site.UID), ResourceUid: string(cs.UID), RuntimeUuid: cs.Spec.RuntimeUUID, PodUid: string(pod.UID), AgentAddress: address, TargetVersion: cs.Status.Target.Version, GiteaWebUrl: site.Status.CanonicalURL}
	for _, endpoint := range cs.Status.Target.Endpoints {
		route.Endpoints = append(route.Endpoints, &componentv1.GatewayEndpoint{EndpointId: endpoint.ID, Label: endpoint.Label, Public: endpoint.Public})
	}
	slices.SortFunc(route.Endpoints, func(a, b *componentv1.GatewayEndpoint) int { return strings.Compare(a.EndpointId, b.EndpointId) })
	return route, nil
}

func (s *ComponentServer) GatewayControl(ctx context.Context, stream componentv1connect.ComponentServiceGatewayControlServerStream) error {
	identity, err := componentIdentityFromContext(ctx, "gateway")
	if err != nil {
		return err
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	if first.ProtocolVersion != 1 || strings.TrimSpace(first.SessionId) == "" || first.Sequence == 0 {
		return connect.NewError(connect.CodeInvalidArgument, "gateway control identity is invalid")
	}
	session := identity.Instance + "/" + first.SessionId
	if err := s.replaceGatewayActivity(ctx, identity, session, first.ActiveSessions); err != nil {
		return err
	}
	defer func() {
		if s.Activity != nil {
			_ = s.Activity.ReplaceGateway(identity.UID, session, nil, true, time.Now())
		}
	}()

	requests := make(chan *componentv1.GatewayControlRequest)
	receiveErrors := make(chan error, 1)
	go func() {
		for {
			request, err := stream.Receive()
			if err != nil {
				receiveErrors <- err
				return
			}
			select {
			case requests <- request:
			case <-ctx.Done():
				return
			}
		}
	}()
	changes := (<-chan struct{})(nil)
	unsubscribe := func() {}
	if s.GatewayChanges != nil {
		changes, unsubscribe = s.GatewayChanges.Subscribe()
	}
	defer unsubscribe()
	permitTicker := time.NewTicker(10 * time.Second)
	defer permitTicker.Stop()
	streamLifetime := time.NewTimer(45 * time.Minute)
	defer streamLifetime.Stop()
	sequence, cursor := first.Sequence, ""
	sendSnapshot := true
	for {
		if sendSnapshot {
			snapshot, err := s.gatewaySnapshot(ctx, identity)
			if err != nil {
				return connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
			}
			if snapshot.Cursor != cursor {
				snapshot.Sequence = sequence
				snapshot.PermitValidForMilliseconds = 30_000
				if err := stream.Send(snapshot); err != nil {
					return err
				}
				cursor = snapshot.Cursor
			}
			sendSnapshot = false
		}
		select {
		case <-ctx.Done():
			return connect.NewError(connect.CodeCanceled, ctx.Err().Error()).WithCause(ctx.Err())
		case err := <-receiveErrors:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case request := <-requests:
			if request.ProtocolVersion != 1 || request.SessionId != first.SessionId || request.Sequence <= sequence {
				return connect.NewError(connect.CodeInvalidArgument, "gateway control sequence is invalid")
			}
			if err := s.replaceGatewayActivity(ctx, identity, session, request.ActiveSessions); err != nil {
				return err
			}
			sequence = request.Sequence
		case <-changes:
			sendSnapshot = true
		case <-permitTicker.C:
			if err := stream.Send(&componentv1.GatewayControlResponse{ProtocolVersion: 1, Sequence: sequence, PermitValidForMilliseconds: 30_000, Cursor: cursor}); err != nil {
				return err
			}
		case <-streamLifetime.C:
			return nil
		}
	}
}

func (s *ComponentServer) replaceGatewayActivity(ctx context.Context, identity componentIdentity, session string, counts map[string]int64) error {
	for runtimeUUID, count := range counts {
		if count < 0 {
			return connect.NewError(connect.CodeInvalidArgument, "gateway activity count is invalid")
		}
		if _, _, err := s.runtimeForGateway(ctx, identity, runtimeUUID); err != nil {
			return connect.NewError(connect.CodeFailedPrecondition, "gateway activity contains a stale Runtime")
		}
	}
	if s.Activity != nil {
		if err := s.Activity.ReplaceGateway(identity.UID, session, counts, false, time.Now()); err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
		}
	}
	return nil
}

func (s *ComponentServer) runtimeForGateway(ctx context.Context, identity componentIdentity, runtimeUUID string) (*api.Codespace, *api.GiteaSite, error) {
	if s.IdentityIndex == nil {
		return nil, nil, fmt.Errorf("gateway runtime identity index is unavailable")
	}
	var rows api.CodespaceList
	if err := s.IdentityIndex.List(ctx, &rows, client.MatchingFields{RuntimeUUIDIndex: runtimeUUID}); err != nil {
		return nil, nil, err
	}
	if len(rows.Items) != 1 {
		return nil, nil, connect.NewError(connect.CodeNotFound, "runtime is not available")
	}
	cs := &rows.Items[0]
	indexedUID := cs.UID
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(cs), cs); err != nil {
		return nil, nil, err
	}
	if cs.UID != indexedUID || cs.Spec.RuntimeUUID != runtimeUUID || !cs.DeletionTimestamp.IsZero() {
		return nil, nil, connect.NewError(connect.CodeNotFound, "runtime is not available")
	}
	var site api.GiteaSite
	if err := s.Client.Get(ctx, types.NamespacedName{Name: cs.Spec.Site.Name}, &site); err != nil {
		return nil, nil, err
	}
	if site.UID != cs.Spec.Site.UID || !site.Spec.Enabled || string(site.Spec.Gateway.UID) != identity.UID || !site.DeletionTimestamp.IsZero() {
		return nil, nil, connect.NewError(connect.CodeNotFound, "runtime is not available")
	}
	return cs, &site, nil
}

func gatewayDenied(category string) *componentv1.AuthorizeGatewayResponse {
	return &componentv1.AuthorizeGatewayResponse{DeniedCategory: category}
}

func (s *ComponentServer) AuthorizeGateway(ctx context.Context, request *componentv1.AuthorizeGatewayRequest) (*componentv1.AuthorizeGatewayResponse, error) {
	identity, err := componentIdentityFromContext(ctx, "gateway")
	if err != nil {
		return nil, err
	}
	if request.ProtocolVersion != 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "unsupported component protocol version")
	}
	if open := request.GetOpenCode(); open != nil {
		if strings.TrimSpace(open.Code) == "" || strings.TrimSpace(open.RuntimeUuid) == "" || strings.TrimSpace(open.EndpointId) == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, "open code target is incomplete")
		}
		_, site, err := s.runtimeForGateway(ctx, identity, open.RuntimeUuid)
		if err != nil {
			return gatewayDenied("runtime_unavailable"), nil
		}
		remote, err := siteManagerClient(ctx, s.Client, s.ManagementNamespace, site)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
		}
		result, err := remote.ValidateOpenToken(ctx, &codespacev1.ValidateOpenTokenRequest{ProtocolVersion: 1, Code: open.Code})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
		}
		if allowed := result.GetAllowed(); allowed != nil {
			if allowed.RuntimeUuid != open.RuntimeUuid || allowed.EndpointId != open.EndpointId {
				return gatewayDenied("target_mismatch"), nil
			}
			if s.Activity != nil {
				s.Activity.ObserveInteraction(string(site.UID), allowed.RuntimeUuid, allowed.InteractionGeneration, time.Now())
			}
			return &componentv1.AuthorizeGatewayResponse{Allowed: true, UserId: allowed.UserId, RuntimeUuid: allowed.RuntimeUuid, EndpointId: allowed.EndpointId}, nil
		}
		if denied := result.GetDenied(); denied != nil {
			return gatewayDenied(denied.Category), nil
		}
		return nil, connect.NewError(connect.CodeInternal, "gitea returned an invalid open authorization outcome")
	}
	var runtimeUUID string
	switch value := request.Request.(type) {
	case *componentv1.AuthorizeGatewayRequest_PublicEndpoint:
		runtimeUUID = value.PublicEndpoint.RuntimeUuid
	case *componentv1.AuthorizeGatewayRequest_SshPublicKey:
		runtimeUUID = value.SshPublicKey.RuntimeUuid
	case *componentv1.AuthorizeGatewayRequest_EndpointSession:
		runtimeUUID = value.EndpointSession.RuntimeUuid
	case *componentv1.AuthorizeGatewayRequest_SshSession:
		runtimeUUID = value.SshSession.RuntimeUuid
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, "gateway authorization request is missing")
	}
	_, site, err := s.runtimeForGateway(ctx, identity, runtimeUUID)
	if err != nil {
		return gatewayDenied("runtime_unavailable"), nil
	}
	remote, err := siteManagerClient(ctx, s.Client, s.ManagementNamespace, site)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
	}
	response := &componentv1.AuthorizeGatewayResponse{RuntimeUuid: runtimeUUID}
	switch value := request.Request.(type) {
	case *componentv1.AuthorizeGatewayRequest_PublicEndpoint:
		result, err := remote.ValidatePublicEndpoint(ctx, &codespacev1.ValidatePublicEndpointRequest{ProtocolVersion: 1, RuntimeUuid: runtimeUUID, EndpointId: value.PublicEndpoint.EndpointId})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
		}
		response.Allowed, response.EndpointId = result.GetAllowed() != nil, value.PublicEndpoint.EndpointId
		if denied := result.GetDenied(); denied != nil {
			response.DeniedCategory = denied.Category
		}
	case *componentv1.AuthorizeGatewayRequest_SshPublicKey:
		result, err := remote.VerifySSHPublicKey(ctx, &codespacev1.VerifySSHPublicKeyRequest{ProtocolVersion: 1, RuntimeUuid: runtimeUUID, PublicKey: value.SshPublicKey.PublicKey})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
		}
		if allowed := result.GetAllowed(); allowed != nil {
			response.Allowed, response.UserId = true, allowed.UserId
			if s.Activity != nil {
				s.Activity.ObserveInteraction(string(site.UID), runtimeUUID, allowed.InteractionGeneration, time.Now())
			}
		}
		if denied := result.GetDenied(); denied != nil {
			response.DeniedCategory = denied.Category
		}
	case *componentv1.AuthorizeGatewayRequest_EndpointSession:
		result, err := remote.RevalidateGatewaySession(ctx, &codespacev1.RevalidateGatewaySessionRequest{ProtocolVersion: 1, Session: &codespacev1.RevalidateGatewaySessionRequest_Endpoint{Endpoint: &codespacev1.EndpointSessionBinding{UserId: value.EndpointSession.UserId, RuntimeUuid: runtimeUUID, EndpointId: value.EndpointSession.EndpointId}}})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
		}
		response.Allowed, response.UserId, response.EndpointId = result.GetAllowed() != nil, value.EndpointSession.UserId, value.EndpointSession.EndpointId
		if denied := result.GetDenied(); denied != nil {
			response.DeniedCategory = denied.Category
		}
	case *componentv1.AuthorizeGatewayRequest_SshSession:
		result, err := remote.RevalidateGatewaySession(ctx, &codespacev1.RevalidateGatewaySessionRequest{ProtocolVersion: 1, Session: &codespacev1.RevalidateGatewaySessionRequest_Ssh{Ssh: &codespacev1.SSHSessionBinding{UserId: value.SshSession.UserId, RuntimeUuid: runtimeUUID}}})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
		}
		response.Allowed, response.UserId = result.GetAllowed() != nil, value.SshSession.UserId
		if denied := result.GetDenied(); denied != nil {
			response.DeniedCategory = denied.Category
		}
	}
	return response, nil
}

func (s *ComponentServer) IssueAgentAccess(ctx context.Context, request *componentv1.IssueAgentAccessRequest) (*componentv1.IssueAgentAccessResponse, error) {
	identity, err := componentIdentityFromContext(ctx, "gateway")
	if err != nil {
		return nil, err
	}
	if request.ProtocolVersion != 1 || request.Capability == agentv1.AccessCapability_ACCESS_CAPABILITY_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, "agent access request is invalid")
	}
	cs, _, err := s.runtimeForGateway(ctx, identity, request.RuntimeUuid)
	if err != nil {
		return nil, err
	}
	// Tickets use live reads; snapshots may lag a stop or Pod replacement.
	route, err := s.route(ctx, s.Client, cs, types.UID(identity.UID))
	if err != nil {
		return nil, err
	}
	if route == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "runtime access target is unavailable")
	}
	if request.Capability == agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT {
		found := false
		for _, endpoint := range cs.Status.Target.Endpoints {
			found = found || endpoint.ID == request.EndpointId
		}
		if !found {
			return nil, connect.NewError(connect.CodeNotFound, "endpoint is not available")
		}
	} else if request.EndpointId != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "endpoint is only valid for endpoint access")
	}
	ticket, err := s.Tickets.Issue(cs, request.Capability, request.EndpointId, time.Minute)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error()).WithCause(err)
	}
	return &componentv1.IssueAgentAccessResponse{Runtime: route, Ticket: ticket}, nil
}
