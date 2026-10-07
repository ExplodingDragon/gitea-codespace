// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	componentv1 "gitea.dev/codespace-proto-go/component/v1"
	"gitea.dev/codespace-proto-go/component/v1/componentv1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	return componentv1connect.NewComponentServiceHandler(s, connect.WithReadMaxBytes(api.MaxObjectBytes), connect.WithSendMaxBytes(api.MaxObjectBytes))
}

func componentIdentityFromContext(ctx context.Context, role string) (componentIdentity, error) {
	identity, ok := ctx.Value(componentIdentityContextKey{}).(componentIdentity)
	if !ok || identity.Role != role || identity.UID == "" || identity.Instance == "" {
		return componentIdentity{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("component identity is not authorized for %s", role))
	}
	return identity, nil
}

func (s *ComponentServer) gatewayConfig(ctx context.Context, identity componentIdentity) (*componentv1.GatewayConfigurationSnapshot, types.UID, error) {
	reader := s.IdentityIndex
	if reader == nil {
		reader = s.Client
	}
	var components corev1.ConfigMapList
	if err := reader.List(ctx, &components, client.InNamespace(s.ManagementNamespace)); err != nil {
		return nil, "", err
	}
	var component *corev1.ConfigMap
	for i := range components.Items {
		candidate := &components.Items[i]
		if string(candidate.UID) == identity.UID && candidate.Labels[ComponentLabel] == "gateway" && candidate.DeletionTimestamp.IsZero() {
			component = candidate
			break
		}
	}
	if component == nil {
		return nil, "", connect.NewError(connect.CodePermissionDenied, fmt.Errorf("gateway component identity is no longer configured"))
	}
	value := func(name, fallback string) string {
		if strings.TrimSpace(component.Data[name]) != "" {
			return strings.TrimSpace(component.Data[name])
		}
		return fallback
	}
	integer := func(name string, fallback int64) (int64, error) {
		if strings.TrimSpace(component.Data[name]) == "" {
			return fallback, nil
		}
		parsed, err := strconv.ParseInt(component.Data[name], 10, 32)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("gateway %s must be a positive integer", name)
		}
		return parsed, nil
	}
	sessionTTL, err := integer("sessionTTLMilliseconds", int64((12*time.Hour)/time.Millisecond))
	if err != nil {
		return nil, "", err
	}
	idleTimeout, err := integer("sessionIdleTimeoutMilliseconds", int64((30*time.Minute)/time.Millisecond))
	if err != nil {
		return nil, "", err
	}
	revalidate, err := integer("revalidateIntervalMilliseconds", int64((30*time.Second)/time.Millisecond))
	if err != nil {
		return nil, "", err
	}
	maxPerCodespace, err := integer("maxSessionsPerCodespace", 16)
	if err != nil {
		return nil, "", err
	}
	maxPerUser, err := integer("maxSessionsPerUser", 32)
	if err != nil {
		return nil, "", err
	}
	maxInflight, err := integer("maxInflight", 256)
	if err != nil {
		return nil, "", err
	}
	maxInflightSession, err := integer("maxInflightPerSession", 16)
	if err != nil {
		return nil, "", err
	}
	maxChannels, err := integer("maxChannelsPerSSHConnection", 16)
	if err != nil {
		return nil, "", err
	}
	return &componentv1.GatewayConfigurationSnapshot{
		HttpListen: value("httpListen", ":8080"), PublicUrl: value("url", ""), SshListen: value("sshListen", ":2222"), SshPublicAddress: value("sshAddress", ""),
		SessionTtlMilliseconds: sessionTTL, SessionIdleTimeoutMilliseconds: idleTimeout, RevalidateIntervalMilliseconds: revalidate,
		MaxSessionsPerCodespace: int32(maxPerCodespace), MaxSessionsPerUser: int32(maxPerUser), MaxInflight: int32(maxInflight), MaxInflightPerSession: int32(maxInflightSession), MaxChannelsPerSshConnection: int32(maxChannels),
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
	if err := s.IdentityIndex.List(ctx, &sites); err != nil {
		return nil, err
	}
	allowedSites := make(map[types.UID]bool)
	for i := range sites.Items {
		site := &sites.Items[i]
		if site.Spec.Enabled && site.Spec.Gateway.UID == componentUID && meta.IsStatusConditionTrue(site.Status.Conditions, "InfrastructureReady") {
			allowedSites[site.UID] = true
		}
	}
	var runtimes api.CodespaceList
	if err := s.IdentityIndex.List(ctx, &runtimes); err != nil {
		return nil, err
	}
	response := &componentv1.GatewayControlResponse{ProtocolVersion: 1, Snapshot: true, Config: configuration}
	for i := range runtimes.Items {
		cs := &runtimes.Items[i]
		if !allowedSites[cs.Spec.Site.UID] || cs.Status.Target == nil || !cs.Status.Target.Ready || cs.Status.Pod.UID == "" {
			continue
		}
		route, err := s.route(ctx, cs)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		response.Runtimes = append(response.Runtimes, route)
	}
	slices.SortFunc(response.Runtimes, func(a, b *componentv1.GatewayRuntimeRoute) int { return strings.Compare(a.RuntimeUuid, b.RuntimeUuid) })
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(response)
	if err != nil {
		return nil, err
	}
	response.Cursor = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return response, nil
}

func (s *ComponentServer) route(ctx context.Context, cs *api.Codespace) (*componentv1.GatewayRuntimeRoute, error) {
	var site api.GiteaSite
	if err := s.IdentityIndex.Get(ctx, types.NamespacedName{Name: cs.Spec.Site.Name}, &site); err != nil {
		return nil, err
	}
	if site.UID != cs.Spec.Site.UID || site.Status.CanonicalURL == "" || !site.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("runtime site identity is not current")
	}
	var pod corev1.Pod
	if err := s.IdentityIndex.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Pod.Name}, &pod); err != nil {
		return nil, err
	}
	if pod.UID != cs.Status.Pod.UID || !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning || !metav1.IsControlledBy(&pod, cs) {
		return nil, fmt.Errorf("runtime Pod identity is not current")
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
		return nil, fmt.Errorf("runtime Pod has no routable IP address")
	}
	route := &componentv1.GatewayRuntimeRoute{SiteUid: string(cs.Spec.Site.UID), ResourceUid: string(cs.UID), RuntimeUuid: cs.Spec.RuntimeUUID, PodUid: string(pod.UID), AgentAddress: address, TargetVersion: cs.Status.Target.Version, GiteaWebUrl: site.Status.CanonicalURL}
	for _, endpoint := range cs.Status.Target.Endpoints {
		route.Endpoints = append(route.Endpoints, &componentv1.GatewayEndpoint{EndpointId: endpoint.ID, Label: endpoint.Label, Public: endpoint.Public})
	}
	slices.SortFunc(route.Endpoints, func(a, b *componentv1.GatewayEndpoint) int { return strings.Compare(a.EndpointId, b.EndpointId) })
	return route, nil
}

func (s *ComponentServer) GatewayControl(ctx context.Context, stream *connect.BidiStream[componentv1.GatewayControlRequest, componentv1.GatewayControlResponse]) error {
	identity, err := componentIdentityFromContext(ctx, "gateway")
	if err != nil {
		return err
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	if first.ProtocolVersion != 1 || strings.TrimSpace(first.SessionId) == "" || first.Sequence == 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("gateway control identity is invalid"))
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
				return connect.NewError(connect.CodeUnavailable, err)
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
			return connect.NewError(connect.CodeCanceled, ctx.Err())
		case err := <-receiveErrors:
			if err == io.EOF {
				return nil
			}
			return err
		case request := <-requests:
			if request.ProtocolVersion != 1 || request.SessionId != first.SessionId || request.Sequence <= sequence {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("gateway control sequence is invalid"))
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
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("gateway activity count is invalid"))
		}
		if _, _, err := s.runtimeForGateway(ctx, identity, runtimeUUID); err != nil {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("gateway activity contains a stale Runtime"))
		}
	}
	if s.Activity != nil {
		if err := s.Activity.ReplaceGateway(identity.UID, session, counts, false, time.Now()); err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err)
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
		return nil, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("runtime is not available"))
	}
	cs := &rows.Items[0]
	indexedUID := cs.UID
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(cs), cs); err != nil {
		return nil, nil, err
	}
	if cs.UID != indexedUID || cs.Spec.RuntimeUUID != runtimeUUID || !cs.DeletionTimestamp.IsZero() {
		return nil, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("runtime is not available"))
	}
	var site api.GiteaSite
	if err := s.Client.Get(ctx, types.NamespacedName{Name: cs.Spec.Site.Name}, &site); err != nil {
		return nil, nil, err
	}
	if site.UID != cs.Spec.Site.UID || !site.Spec.Enabled || string(site.Spec.Gateway.UID) != identity.UID || !site.DeletionTimestamp.IsZero() {
		return nil, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("runtime is not available"))
	}
	return cs, &site, nil
}

func gatewayDenied(category string) *connect.Response[componentv1.AuthorizeGatewayResponse] {
	return connect.NewResponse(&componentv1.AuthorizeGatewayResponse{DeniedCategory: category})
}

func (s *ComponentServer) AuthorizeGateway(ctx context.Context, request *connect.Request[componentv1.AuthorizeGatewayRequest]) (*connect.Response[componentv1.AuthorizeGatewayResponse], error) {
	identity, err := componentIdentityFromContext(ctx, "gateway")
	if err != nil {
		return nil, err
	}
	if request.Msg.ProtocolVersion != 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unsupported component protocol version"))
	}
	if open := request.Msg.GetOpenCode(); open != nil {
		if strings.TrimSpace(open.Code) == "" || strings.TrimSpace(open.RuntimeUuid) == "" || strings.TrimSpace(open.EndpointId) == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("open code target is incomplete"))
		}
		_, site, err := s.runtimeForGateway(ctx, identity, open.RuntimeUuid)
		if err != nil {
			return gatewayDenied("runtime_unavailable"), nil
		}
		remote, err := siteManagerClient(ctx, s.Client, s.ManagementNamespace, site)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		result, err := remote.ValidateOpenToken(ctx, connect.NewRequest(&codespacev1.ValidateOpenTokenRequest{ProtocolVersion: 1, Code: open.Code}))
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		if allowed := result.Msg.GetAllowed(); allowed != nil {
			if allowed.RuntimeUuid != open.RuntimeUuid || allowed.EndpointId != open.EndpointId {
				return gatewayDenied("target_mismatch"), nil
			}
			if s.Activity != nil {
				s.Activity.ObserveInteraction(string(site.UID), allowed.RuntimeUuid, allowed.InteractionGeneration, time.Now())
			}
			return connect.NewResponse(&componentv1.AuthorizeGatewayResponse{Allowed: true, UserId: allowed.UserId, RuntimeUuid: allowed.RuntimeUuid, EndpointId: allowed.EndpointId}), nil
		}
		if denied := result.Msg.GetDenied(); denied != nil {
			return gatewayDenied(denied.Category), nil
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("gitea returned an invalid open authorization outcome"))
	}
	var runtimeUUID string
	switch value := request.Msg.Request.(type) {
	case *componentv1.AuthorizeGatewayRequest_PublicEndpoint:
		runtimeUUID = value.PublicEndpoint.RuntimeUuid
	case *componentv1.AuthorizeGatewayRequest_SshPublicKey:
		runtimeUUID = value.SshPublicKey.RuntimeUuid
	case *componentv1.AuthorizeGatewayRequest_EndpointSession:
		runtimeUUID = value.EndpointSession.RuntimeUuid
	case *componentv1.AuthorizeGatewayRequest_SshSession:
		runtimeUUID = value.SshSession.RuntimeUuid
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("gateway authorization request is missing"))
	}
	_, site, err := s.runtimeForGateway(ctx, identity, runtimeUUID)
	if err != nil {
		return gatewayDenied("runtime_unavailable"), nil
	}
	remote, err := siteManagerClient(ctx, s.Client, s.ManagementNamespace, site)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	response := &componentv1.AuthorizeGatewayResponse{RuntimeUuid: runtimeUUID}
	switch value := request.Msg.Request.(type) {
	case *componentv1.AuthorizeGatewayRequest_PublicEndpoint:
		result, err := remote.ValidatePublicEndpoint(ctx, connect.NewRequest(&codespacev1.ValidatePublicEndpointRequest{ProtocolVersion: 1, RuntimeUuid: runtimeUUID, EndpointId: value.PublicEndpoint.EndpointId}))
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		response.Allowed, response.EndpointId = result.Msg.GetAllowed() != nil, value.PublicEndpoint.EndpointId
		if denied := result.Msg.GetDenied(); denied != nil {
			response.DeniedCategory = denied.Category
		}
	case *componentv1.AuthorizeGatewayRequest_SshPublicKey:
		result, err := remote.VerifySSHPublicKey(ctx, connect.NewRequest(&codespacev1.VerifySSHPublicKeyRequest{ProtocolVersion: 1, RuntimeUuid: runtimeUUID, PublicKey: value.SshPublicKey.PublicKey}))
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		if allowed := result.Msg.GetAllowed(); allowed != nil {
			response.Allowed, response.UserId = true, allowed.UserId
			if s.Activity != nil {
				s.Activity.ObserveInteraction(string(site.UID), runtimeUUID, allowed.InteractionGeneration, time.Now())
			}
		}
		if denied := result.Msg.GetDenied(); denied != nil {
			response.DeniedCategory = denied.Category
		}
	case *componentv1.AuthorizeGatewayRequest_EndpointSession:
		result, err := remote.RevalidateGatewaySession(ctx, connect.NewRequest(&codespacev1.RevalidateGatewaySessionRequest{ProtocolVersion: 1, Session: &codespacev1.RevalidateGatewaySessionRequest_Endpoint{Endpoint: &codespacev1.EndpointSessionBinding{UserId: value.EndpointSession.UserId, RuntimeUuid: runtimeUUID, EndpointId: value.EndpointSession.EndpointId}}}))
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		response.Allowed, response.UserId, response.EndpointId = result.Msg.GetAllowed() != nil, value.EndpointSession.UserId, value.EndpointSession.EndpointId
		if denied := result.Msg.GetDenied(); denied != nil {
			response.DeniedCategory = denied.Category
		}
	case *componentv1.AuthorizeGatewayRequest_SshSession:
		result, err := remote.RevalidateGatewaySession(ctx, connect.NewRequest(&codespacev1.RevalidateGatewaySessionRequest{ProtocolVersion: 1, Session: &codespacev1.RevalidateGatewaySessionRequest_Ssh{Ssh: &codespacev1.SSHSessionBinding{UserId: value.SshSession.UserId, RuntimeUuid: runtimeUUID}}}))
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		response.Allowed, response.UserId = result.Msg.GetAllowed() != nil, value.SshSession.UserId
		if denied := result.Msg.GetDenied(); denied != nil {
			response.DeniedCategory = denied.Category
		}
	}
	return connect.NewResponse(response), nil
}

func (s *ComponentServer) IssueAgentAccess(ctx context.Context, request *connect.Request[componentv1.IssueAgentAccessRequest]) (*connect.Response[componentv1.IssueAgentAccessResponse], error) {
	identity, err := componentIdentityFromContext(ctx, "gateway")
	if err != nil {
		return nil, err
	}
	if request.Msg.ProtocolVersion != 1 || request.Msg.Capability == agentv1.AccessCapability_ACCESS_CAPABILITY_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent access request is invalid"))
	}
	cs, _, err := s.runtimeForGateway(ctx, identity, request.Msg.RuntimeUuid)
	if err != nil {
		return nil, err
	}
	if request.Msg.Capability == agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT {
		found := false
		for _, endpoint := range cs.Status.Target.Endpoints {
			found = found || endpoint.ID == request.Msg.EndpointId
		}
		if !found {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("endpoint is not available"))
		}
	} else if request.Msg.EndpointId != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("endpoint is only valid for endpoint access"))
	}
	route, err := s.route(ctx, cs)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	ticket, err := s.Tickets.Issue(cs, request.Msg.Capability, request.Msg.EndpointId, time.Minute)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&componentv1.IssueAgentAccessResponse{Runtime: route, Ticket: ticket}), nil
}
