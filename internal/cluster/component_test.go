// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/accessticket"
	cachepkg "gitea.dev/codespace/internal/cache"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	configpkg "gitea.dev/codespace/internal/config"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"gitea.dev/codespace/internal/rpc/component/v1/componentv1connect"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type openAuthorizationRemote struct {
	codespacev1connect.UnimplementedManagerServiceHandler
	calls   int
	binding *codespacev1.OpenTokenBinding
}

func (r *openAuthorizationRemote) ValidateOpenToken(context.Context, *codespacev1.ValidateOpenTokenRequest) (*codespacev1.ValidateOpenTokenResponse, error) {
	r.calls++
	if r.binding == nil {
		return &codespacev1.ValidateOpenTokenResponse{Outcome: &codespacev1.ValidateOpenTokenResponse_Denied{Denied: &codespacev1.FailureDetail{Category: "invalid_code"}}}, nil
	}
	return &codespacev1.ValidateOpenTokenResponse{Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{Allowed: r.binding}}, nil
}

func serveManagerRemote(t *testing.T, remote codespacev1connect.ManagerServiceHandler) *httptest.Server {
	t.Helper()
	rpcServer := connect.NewServer()
	codespacev1connect.RegisterManagerServiceHandler(rpcServer, remote)
	handler := http.NewServeMux()
	connecthttp.Mount(handler, rpcServer)
	path := "/" + codespacev1connect.ManagerServiceName + "/"

	mux := http.NewServeMux()
	mux.Handle("/api/codespace"+path, http.StripPrefix("/api/codespace", handler))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestGatewayOpenCodeUsesOnlyTargetRuntimeSite(t *testing.T) {
	const runtimeKey = "target-site/1"
	target := &openAuthorizationRemote{binding: &codespacev1.OpenTokenBinding{UserId: 1, CodespaceId: 1, EndpointId: "workspace", InteractionGeneration: 3}}
	unrelated := &openAuthorizationRemote{}
	targetServer, unrelatedServer := serveManagerRemote(t, target), serveManagerRemote(t, unrelated)

	site := testSite("target", "target-site")
	site.Spec.URL = targetServer.URL
	site.Status.CanonicalURL = "https://target.gitea.example.com"
	site.Spec.Credential = api.ResourceReference{Name: "target-credential", UID: "target-credential-uid"}
	unrelatedSite := testSite("unrelated", "unrelated-site")
	unrelatedSite.Spec.URL = unrelatedServer.URL
	unrelatedSite.Status.CanonicalURL = "https://unrelated.gitea.example.com"
	unrelatedSite.Spec.Credential = api.ResourceReference{Name: "unrelated-credential", UID: "unrelated-credential-uid"}
	credential := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: site.Spec.Credential.Name, Namespace: "codespace-system", UID: site.Spec.Credential.UID, Labels: map[string]string{SiteUIDLabel: string(site.UID)}}, Data: map[string][]byte{"managerSecret": []byte("target")}}
	unrelatedCredential := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: unrelatedSite.Spec.Credential.Name, Namespace: "codespace-system", UID: unrelatedSite.Spec.Credential.UID, Labels: map[string]string{SiteUIDLabel: string(unrelatedSite.UID)}}, Data: map[string][]byte{"managerSecret": []byte("unrelated")}}
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-target", UID: "runtime-uid"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}, CodespaceID: 1}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(site, unrelatedSite, credential, unrelatedCredential, cs).Build()
	index := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cs).WithIndex(&api.Codespace{}, RuntimeKeyIndex, func(object client.Object) []string {
		return []string{object.(*api.Codespace).RuntimeKey()}
	}).Build()
	tracker := &RuntimeActivityTracker{}
	server := &ComponentServer{Client: c, IdentityIndex: index, ManagementNamespace: "codespace-system", Activity: tracker, Connections: newSiteConnectionStore(c, "codespace-system")}
	ctx := contextWithComponent(t.Context(), componentIdentity{Role: "gateway", UID: "gateway-uid"})

	response, err := server.AuthorizeGateway(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_OpenCode{OpenCode: &componentv1.OpenCodeAuthorization{Code: "secret", RuntimeKey: runtimeKey, EndpointId: "workspace"}}})
	require.NoError(t, err)
	require.True(t, response.Allowed)
	require.Equal(t, string(site.UID), response.SiteUid)
	require.Equal(t, 1, target.calls)
	require.Zero(t, unrelated.calls)

	response, err = server.AuthorizeGateway(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_EndpointSession{EndpointSession: &componentv1.EndpointSessionAuthorization{SiteUid: "another-site", UserId: 1, RuntimeKey: runtimeKey, EndpointId: "workspace"}}})
	require.NoError(t, err)
	require.False(t, response.Allowed)
	require.Equal(t, "site_mismatch", response.DeniedCategory)

	target.binding.EndpointId = "other"
	response, err = server.AuthorizeGateway(ctx, &componentv1.AuthorizeGatewayRequest{Request: &componentv1.AuthorizeGatewayRequest_OpenCode{OpenCode: &componentv1.OpenCodeAuthorization{Code: "secret", RuntimeKey: runtimeKey, EndpointId: "workspace"}}})
	require.NoError(t, err)
	require.False(t, response.Allowed)
	require.Equal(t, "target_mismatch", response.DeniedCategory)
	require.Zero(t, unrelated.calls)
}

func TestGatewaySnapshotAndAccessTicketUseCurrentPodIdentity(t *testing.T) {
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	encoded, err := x509.MarshalPKCS8PrivateKey(hostPrivate)
	require.NoError(t, err)
	hostKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	component := testGatewayComponent("gateway", "gateway-uid")
	component.Spec.Gateway.EndpointBaseURL = "https://workspace.example"
	component.Spec.Gateway.SSHExternalAddress = "workspace.example:2222"
	secret := testComponentCredential(component, map[string][]byte{"hostKey": hostKey})
	site := testSite("example", "site-uid")
	site.Status.CanonicalURL = "https://gitea.example"
	site.Status.Conditions = []metav1.Condition{{Type: "InfrastructureReady", Status: metav1.ConditionTrue, ObservedGeneration: site.Generation}}
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "resource-uid"},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}, CodespaceID: 1},
		Status: api.CodespaceStatus{
			Pod:    api.ObservedResource{Name: "runtime-pod", UID: "pod-uid"},
			Target: &api.AgentTarget{Version: 4, Ready: true, PrimaryContainerID: "container", WorkspaceFolder: "/workspaces/project", Endpoints: []api.AgentEndpoint{{ID: "workspace", Label: "Workspace", ContainerID: "container", Port: 13337}}},
		},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cs.Status.Pod.Name, Namespace: cs.Namespace, UID: cs.Status.Pod.UID, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cs, api.GroupVersion.WithKind("Codespace"))}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.2.3", PodIPs: []corev1.PodIP{{IP: "10.1.2.3"}}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, secret, site, cs, pod).Build()
	index := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, secret, site, cs, pod).
		WithIndex(&api.CodespaceComponent{}, IdentityUIDIndex, func(object client.Object) []string { return []string{string(object.GetUID())} }).
		WithIndex(&api.GiteaSite{}, SiteGatewayUIDIndex, func(object client.Object) []string { return []string{string(object.(*api.GiteaSite).Spec.Gateway.UID)} }).
		WithIndex(&api.Codespace{}, RuntimeSiteUIDIndex, func(object client.Object) []string { return []string{string(object.(*api.Codespace).Spec.Site.UID)} }).
		WithIndex(&api.Codespace{}, RuntimeKeyIndex, func(object client.Object) []string { return []string{object.(*api.Codespace).RuntimeKey()} }).Build()
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	server := &ComponentServer{Client: c, IdentityIndex: index, ManagementNamespace: "codespace-system", Tickets: &AccessTickets{privateKey: signingKey}, Changes: &ChangeNotifier{}}
	ctx := contextWithComponent(t.Context(), componentIdentity{Role: "gateway", UID: string(component.UID)})
	snapshot, err := server.gatewaySnapshot(ctx, componentIdentity{Role: "gateway", UID: string(component.UID)})
	require.NoError(t, err)
	require.EqualValues(t, 30_000, snapshot.Config.SshHandshakeTimeoutMilliseconds)
	require.EqualValues(t, 30, snapshot.Config.SshAuthMaxAttemptsPerIp)
	require.EqualValues(t, 20, snapshot.Config.SshAuthMaxAttemptsPerCodespace)
	require.EqualValues(t, 10, snapshot.Config.SshAuthMaxAttemptsPerIpCodespace)
	require.EqualValues(t, 30, snapshot.Config.SshAuthMaxAttemptsPerPublicKey)
	require.EqualValues(t, 600_000, snapshot.Config.SshAuthFailureWindowMilliseconds)
	require.EqualValues(t, 64, snapshot.Config.PublicMaxConnectionsPerEndpoint)
	require.EqualValues(t, 16, snapshot.Config.PublicMaxConnectionsPerIp)
	require.EqualValues(t, 128, snapshot.Config.ValidationMaxInflight)
	require.Len(t, snapshot.Runtimes, 1)
	route := snapshot.Runtimes[0]
	require.Equal(t, "10.1.2.3:8444", route.AgentAddress)
	require.Equal(t, "pod-uid", route.PodUid)
	require.EqualValues(t, 4, route.TargetVersion)

	issued, err := server.IssueAgentAccess(ctx, &componentv1.IssueAgentAccessRequest{RuntimeKey: cs.RuntimeKey(), Capability: agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT, EndpointId: "workspace"})
	require.NoError(t, err)
	claims, err := accessticket.Verify(signingKey.Public().(ed25519.PublicKey), issued.Ticket, time.Now())
	require.NoError(t, err)
	require.Equal(t, string(cs.UID), claims.ResourceUid)
	require.Equal(t, string(pod.UID), claims.PodUid)
	require.EqualValues(t, cs.Status.Target.Version, claims.TargetVersion)

	t.Run("control publishes routes and confirms workspace activity", func(t *testing.T) {
		identity := componentIdentity{Role: "gateway", UID: string(component.UID)}
		rpcServer := connect.NewServer()
		componentv1connect.RegisterComponentServiceHandler(rpcServer, server)
		mux := http.NewServeMux()
		connecthttp.Mount(mux, rpcServer)
		httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mux.ServeHTTP(w, r.WithContext(contextWithComponent(r.Context(), identity)))
		}))
		httpServer.EnableHTTP2 = true
		httpServer.StartTLS()
		defer httpServer.Close()
		remote := componentv1connect.NewComponentServiceClient(connect.NewClient(connecthttp.NewTransport(httpServer.Client(), httpServer.URL, connecthttp.WithGRPC())))
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		stream, err := remote.GatewayControl(ctx)
		require.NoError(t, err)
		defer func() { _ = stream.Close() }()
		require.NoError(t, stream.Send(&componentv1.GatewayControlRequest{SessionId: "gateway-process", Sequence: 1}))
		response, err := stream.Receive()
		require.NoError(t, err)
		require.NotNil(t, response.Config)
		require.EqualValues(t, 1, response.Sequence)
		require.Len(t, response.Runtimes, 1)

		activity := &componentv1.WorkspaceSessionActivity{
			Identity: &componentv1.WorkspaceSessionIdentity{SessionId: "browser-session", SiteUid: string(site.UID), UserId: 1, RuntimeKey: cs.RuntimeKey()},
			Active:   true, LastActiveUnixMilliseconds: time.Now().Add(-24 * time.Hour).UnixMilli(),
		}
		require.NoError(t, stream.Send(&componentv1.GatewayControlRequest{SessionId: "gateway-process", Sequence: 2, ActiveSessions: map[string]int64{cs.RuntimeKey(): 1}, WorkspaceSessions: []*componentv1.WorkspaceSessionActivity{activity}}))
		response, err = stream.Receive()
		require.NoError(t, err)
		require.Nil(t, response.Config)
		require.EqualValues(t, 2, response.Sequence)
		require.Positive(t, response.PermitValidForMilliseconds)
		lookup, err := remote.LookupWorkspaceSession(ctx, &componentv1.LookupWorkspaceSessionRequest{Identity: activity.Identity})
		require.NoError(t, err)
		require.True(t, lookup.Renewable)

		updated := cs.DeepCopy()
		require.NoError(t, index.Get(ctx, client.ObjectKeyFromObject(cs), updated))
		updated.Status.Target.Version++
		require.NoError(t, index.Update(ctx, updated))
		server.Changes.Notify(component.UID)
		response, err = stream.Receive()
		require.NoError(t, err)
		require.NotNil(t, response.Config)
		require.EqualValues(t, 2, response.Sequence)
		require.Positive(t, response.PermitValidForMilliseconds)
		require.Len(t, response.Runtimes, 1)
		require.Equal(t, updated.Status.Target.Version, response.Runtimes[0].TargetVersion)
		updated.Status.Target = nil
		require.NoError(t, index.Update(ctx, updated))
		server.Changes.Notify(component.UID)
		response, err = stream.Receive()
		require.NoError(t, err)
		require.NotNil(t, response.Config)
		require.Empty(t, response.Runtimes)

		require.NoError(t, index.Get(ctx, client.ObjectKeyFromObject(cs), updated))
		updated.Status = cs.Status
		require.NoError(t, index.Update(ctx, updated))
		server.Changes.Notify(component.UID)
		response, err = stream.Receive()
		require.NoError(t, err)
		require.NotNil(t, response.Config)
		require.Len(t, response.Runtimes, 1)

		activity.Active = false
		activity.LastActiveUnixMilliseconds = time.Now().UnixMilli()
		require.NoError(t, stream.Send(&componentv1.GatewayControlRequest{SessionId: "gateway-process", Sequence: 3, WorkspaceSessions: []*componentv1.WorkspaceSessionActivity{activity}}))
		response, err = stream.Receive()
		require.NoError(t, err)
		require.EqualValues(t, 3, response.Sequence)
		require.NoError(t, stream.CloseSend())
		_, err = stream.Receive()
		require.ErrorIs(t, err, io.EOF)
		lookup, err = remote.LookupWorkspaceSession(ctx, &componentv1.LookupWorkspaceSessionRequest{Identity: activity.Identity})
		require.NoError(t, err)
		require.True(t, lookup.Renewable)

		reconnected, err := remote.GatewayControl(ctx)
		require.NoError(t, err)
		defer func() { _ = reconnected.Close() }()
		require.NoError(t, reconnected.Send(&componentv1.GatewayControlRequest{SessionId: "gateway-process", Sequence: 1}))
		response, err = reconnected.Receive()
		require.NoError(t, err)
		require.NotNil(t, response.Config)
		require.Len(t, response.Runtimes, 1)
		require.Equal(t, cs.RuntimeKey(), response.Runtimes[0].RuntimeKey)
	})

	request := &componentv1.IssueAgentAccessRequest{RuntimeKey: cs.RuntimeKey(), Capability: agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT, EndpointId: "workspace"}
	current := cs.DeepCopy()
	current.Status.Target = nil
	require.NoError(t, c.Update(ctx, current))
	_, err = server.IssueAgentAccess(ctx, request)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cs), current))
	current.Status = cs.Status
	require.NoError(t, c.Update(ctx, current))

	// A stale index cannot authorize a replacement Pod.
	require.NoError(t, c.Delete(ctx, pod))
	replacement := pod.DeepCopy()
	replacement.ResourceVersion, replacement.UID = "", "replacement-pod"
	require.NoError(t, c.Create(ctx, replacement))
	_, err = server.IssueAgentAccess(ctx, request)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))

	for _, unavailable := range []string{"missing target", "stopping", "missing pod", "replaced pod", "no pod address"} {
		t.Run(unavailable, func(t *testing.T) {
			other := cs.DeepCopy()
			other.Name, other.UID, other.ResourceVersion = "other", "other-resource", ""
			other.Spec.CodespaceID = 2
			other.Status.Pod.Name = "other-pod"
			otherPod := pod.DeepCopy()
			otherPod.Name, otherPod.ResourceVersion = "other-pod", ""
			otherPod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(other, api.GroupVersion.WithKind("Codespace"))}
			switch unavailable {
			case "missing target":
				other.Status.Target = nil
			case "stopping":
				other.Spec.Operation.Type = "stop"
			case "replaced pod":
				otherPod.UID = "new-pod"
			case "no pod address":
				otherPod.Status.PodIP, otherPod.Status.PodIPs = "", nil
			}
			require.NoError(t, index.Create(ctx, other))
			if unavailable != "missing pod" {
				require.NoError(t, index.Create(ctx, otherPod))
			}
			result, err := server.gatewaySnapshot(ctx, componentIdentity{Role: "gateway", UID: string(component.UID)})
			require.NoError(t, err)
			require.Len(t, result.Runtimes, 1)
			require.Equal(t, cs.RuntimeKey(), result.Runtimes[0].RuntimeKey)
			require.NoError(t, index.Delete(ctx, other))
			if unavailable != "missing pod" {
				require.NoError(t, index.Delete(ctx, otherPod))
			}
		})
	}

	server.IdentityIndex = interceptor.NewClient(index, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				return errors.New("apiserver read failed")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	_, err = server.gatewaySnapshot(ctx, componentIdentity{Role: "gateway", UID: string(component.UID)})
	require.ErrorContains(t, err, "apiserver read failed")
	server.IdentityIndex = index

	require.NoError(t, c.Delete(t.Context(), component))
	component.ResourceVersion, component.UID = "", types.UID("replacement")
	require.NoError(t, c.Create(t.Context(), component))
	_, err = server.gatewaySnapshot(ctx, componentIdentity{Role: "gateway", UID: string(component.UID)})
	require.Error(t, err)
}

func contextWithComponent(ctx context.Context, identity componentIdentity) context.Context {
	return context.WithValue(ctx, componentIdentityContextKey{}, identity)
}

func TestGatewayActivityUsesAuthorizedSnapshot(t *testing.T) {
	tracker := &RuntimeActivityTracker{}
	server := &ComponentServer{Activity: tracker}
	identity := componentIdentity{UID: "gateway-uid"}

	err := server.replaceGatewayActivity(identity, "session", map[string]int64{"current": 2, "stale": 3}, map[string]string{"current": "site"})
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"current": 2}, tracker.gateways[gatewayActivityKey{componentUID: identity.UID, instance: "session"}].counts)

	err = server.replaceGatewayActivity(identity, "session", map[string]int64{"current": -1}, map[string]string{"current": "site"})
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestCacheComponentRuntimeConfig(t *testing.T) {
	for _, storage := range []api.CacheStorage{
		{Driver: "filesystem", VolumeSize: "1Gi", MinFreeSpace: "128MiB"},
		{Driver: "s3", S3: api.CacheS3Storage{Endpoint: "https://storage.example.com", Region: "test", Bucket: "cache", Prefix: "registry", ForcePathStyle: true}},
	} {
		t.Run(storage.Driver, func(t *testing.T) {
			component := testCacheComponent("cache-primary", "cache-uid")
			component.Generation = 3
			component.Spec.Cache.Storage = storage
			require.NoError(t, component.Validate())
			configuration, err := cacheComponentConfig(component)
			require.NoError(t, err)
			require.Equal(t, configpkg.CacheConfig{
				ID: "cache-primary", Name: "Build cache", Revision: 3, Enabled: true,
				Listen: cacheListen, MaxSize: "768MiB", MaxAge: configpkg.Duration(24 * time.Hour), GCInterval: configpkg.Duration(time.Hour),
				Upstreams: map[string]configpkg.RuntimeCacheUpstreamConfig{"docker.io": {Allow: []string{"library/*"}}},
				Storage: configpkg.CacheStorageConfig{
					Driver: storage.Driver, Path: cacheStoragePath, MinFreeSpace: storage.MinFreeSpace,
					S3: configpkg.CacheS3Config{
						Endpoint: storage.S3.Endpoint, Region: storage.S3.Region, Bucket: storage.S3.Bucket,
						Prefix: storage.S3.Prefix, ForcePathStyle: storage.S3.ForcePathStyle,
					},
				},
			}, configuration)
			configuration.Upstreams["docker.io"].Allow[0] = "other/*"
			require.Equal(t, []string{"library/*"}, component.Spec.Cache.Upstreams["docker.io"].Allow)

			for _, field := range []*string{&component.Spec.Cache.MaxAge, &component.Spec.Cache.GCInterval} {
				original := *field
				*field = "invalid"
				_, err := cacheComponentConfig(component)
				require.Error(t, err)
				require.EqualError(t, component.Validate(), err.Error())
				*field = original
			}
		})
	}
}

func TestCacheControlAndRuntimeCredential(t *testing.T) {
	ctx := t.Context()
	namespace := "codespace-system"
	component := testCacheComponent("cache", "cache-uid")
	component.Generation = 7
	component.Spec.Cache.Upstreams = map[string]api.CacheUpstream{"ghcr.io:5443": {Allow: []string{"devcontainers/*"}}}
	secret := testComponentCredential(component, map[string][]byte{
		"registryKey": []byte(strings.Repeat("r", 32)), "tokenKey": []byte(strings.Repeat("t", 32)),
	})
	site := testSite("example", "site-uid")
	site.Spec.Caches = []api.ResourceReference{{Name: "offline-cache", UID: "offline-cache-uid"}, {Name: component.Name, UID: component.UID}}
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"}, Spec: api.CodespaceSpec{
		Site: api.ResourceReference{Name: site.Name, UID: site.UID}, Runtime: testRuntime(),
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, secret, site, cs).Build()
	identityIndex := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component).
		WithIndex(&api.CodespaceComponent{}, IdentityUIDIndex, func(object client.Object) []string { return []string{string(object.GetUID())} }).Build()
	server := &ComponentServer{Client: c, IdentityIndex: identityIndex, ManagementNamespace: namespace}
	cacheContext := contextWithComponent(ctx, componentIdentity{Role: "cache", UID: string(component.UID)})

	identity, material, err := server.cacheForRequest(cacheContext)
	require.NoError(t, err)
	require.EqualValues(t, component.Generation, material.config.Revision)

	sessionID := "11111111-1111-4111-8111-111111111111"
	material.config.Enabled = false
	err = server.claimCache(cacheContext, identity, material, sessionID)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	material.config.Enabled = true
	err = server.claimCache(cacheContext, identity, material, sessionID)
	require.NoError(t, err)
	var ownerLease coordinationv1.Lease
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "cache-owner-" + component.Name}, &ownerLease))
	require.True(t, metav1.IsControlledBy(&ownerLease, component))
	err = server.claimCache(cacheContext, identity, material, "22222222-2222-4222-8222-222222222222")
	require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
	for _, step := range []struct {
		name   string
		status *componentv1.CacheStatus
		bytes  string
		mirror string
		result string
	}{
		{"heartbeat", &componentv1.CacheStatus{CacheBytes: 10, MirrorBytes: 20, CleanupResult: "ok"}, "10", "20", "ok"},
		{"maintenance", nil, "10", "20", "ok"},
		{"empty heartbeat", &componentv1.CacheStatus{}, "0", "0", ""},
	} {
		require.NoError(t, server.renewCache(cacheContext, identity, material, sessionID, step.status), step.name)
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&ownerLease), &ownerLease))
		require.Equal(t, step.bytes, ownerLease.Annotations[cacheBytesAnnotation], step.name)
		require.Equal(t, step.mirror, ownerLease.Annotations[cacheMirrorBytesAnnotation], step.name)
		require.Equal(t, step.result, ownerLease.Annotations[cacheCleanupResultAnnotation], step.name)
	}
	material.config.Enabled = false
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(server.renewCache(cacheContext, identity, material, sessionID, nil)))
	material.config.Enabled = true

	operation := &codespacev1.OperationPayload{Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{
		Repository: &codespacev1.RepositoryCheckout{RepositoryId: 7}, GitIdentity: &codespacev1.GitIdentity{UserId: 9},
	}}}
	runtimeCache, err := server.runtimeCache(ctx, cs, operation)
	require.NoError(t, err)
	cacheNamespace := cachepkg.Namespace(secret.Data["registryKey"], string(site.UID), 7, 9)
	require.Equal(t, "https://cache.codespace-system.svc/cache/"+cacheNamespace, runtimeCache.BuildRegistry)
	require.Equal(t, "https://cache.codespace-system.svc/mirror/"+configpkg.CacheUpstreamID("ghcr.io:5443"), runtimeCache.Mirrors["ghcr.io:5443"])
	credential := runtimeCache.Credentials["cache.codespace-system.svc"]
	require.NotNil(t, credential)
	require.Equal(t, cachepkg.Username, credential.Username)
	verified, err := cachepkg.VerifyCredential(secret.Data["tokenKey"], component.Name, credential.Password, time.Now())
	require.NoError(t, err)
	require.Equal(t, cacheNamespace, verified.Namespace)
	require.True(t, verified.Policy.Build)

	var gcLease coordinationv1.Lease
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "cache-gc-" + component.Name}, &gcLease))
	fallback := testCacheComponent("cache-fallback", "cache-fallback-uid")
	fallbackLease := ownerLease.DeepCopy()
	fallbackLease.Name, fallbackLease.ResourceVersion = cacheOwnerLeaseName(fallback.Name), ""
	fallbackLease.Labels[ComponentUIDLabel] = string(fallback.UID)
	fallbackLease.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(fallback, api.GroupVersion.WithKind("CodespaceComponent"))}
	fallbackLease.Spec.HolderIdentity = ptr.To(cacheOwnerHolder(componentIdentity{UID: string(fallback.UID)}, sessionID, fallback.Generation))
	for _, scenario := range []string{"next cache", "without cache", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			currentSite := site.DeepCopy()
			if scenario == "next cache" {
				currentSite.Spec.Caches = append(currentSite.Spec.Caches, api.ResourceReference{Name: fallback.Name, UID: fallback.UID})
			}
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			isolated := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(component, secret, currentSite, &ownerLease, &gcLease, fallback, testComponentCredential(fallback, secret.Data), fallbackLease).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if obj.GetName() == "cache-gc-"+component.Name {
							if scenario == "canceled" {
								cancel()
							}
							return errors.New("cache Lease update failed")
						}
						return underlying.Update(ctx, obj, opts...)
					},
				}).Build()
			failing := &ComponentServer{ManagementNamespace: namespace, Client: isolated}
			selected, err := failing.runtimeCache(requestCtx, cs, operation)
			if scenario == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			if scenario == "next cache" {
				require.NotNil(t, selected)
				require.Contains(t, selected.BuildRegistry, "https://cache-fallback.codespace-system.svc/cache/")
				require.NotContains(t, selected.Credentials, "cache.codespace-system.svc")
			} else {
				require.Nil(t, selected)
			}
		})
	}

	grant, err := server.beginCacheGC(cacheContext, identity, material, sessionID)
	require.NoError(t, err)
	require.Equal(t, verified.ExpiresAt.UnixMilli(), grant.ReadyAtUnixMilliseconds)
	require.GreaterOrEqual(t, grant.ExpiresAtUnixMilliseconds-grant.ReadyAtUnixMilliseconds, cacheGCTTL.Milliseconds())
	repeatedGrant, err := server.beginCacheGC(cacheContext, identity, material, sessionID)
	require.NoError(t, err)
	require.Equal(t, grant, repeatedGrant)
	runtimeCache, err = server.runtimeCache(ctx, cs, operation)
	require.NoError(t, err)
	require.Nil(t, runtimeCache)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "cache-gc-" + component.Name}, &gcLease))
	require.True(t, metav1.IsControlledBy(&gcLease, component))

	// Replacing both processes preserves the drain cutoff and fences the old owner.
	server = &ComponentServer{Client: c, IdentityIndex: identityIndex, ManagementNamespace: namespace}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&ownerLease), &ownerLease))
	expired := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	ownerLease.Spec.RenewTime = &expired
	require.NoError(t, c.Update(ctx, &ownerLease))
	previousSession := sessionID
	sessionID = "replacement-session"
	require.NoError(t, server.claimCache(cacheContext, identity, material, sessionID))
	resumedGrant, err := server.beginCacheGC(cacheContext, identity, material, sessionID)
	require.NoError(t, err)
	require.NotEqual(t, grant.Id, resumedGrant.Id)
	require.Equal(t, grant.ReadyAtUnixMilliseconds, resumedGrant.ReadyAtUnixMilliseconds)
	require.Equal(t, grant.ExpiresAtUnixMilliseconds, resumedGrant.ExpiresAtUnixMilliseconds)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(server.completeCacheGC(ctx, component, previousSession, grant.Id)))
	grant = resumedGrant
	err = server.completeCacheGC(cacheContext, component, sessionID, grant.Id)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&gcLease), &gcLease))
	protectedUntil, err := cacheCredentialDeadline(&gcLease)
	require.NoError(t, err)
	require.WithinDuration(t, verified.ExpiresAt, protectedUntil, 0)
	notDue, err := server.beginCacheGC(cacheContext, identity, material, sessionID)
	require.NoError(t, err)
	require.Nil(t, notDue)
	gcLease.Annotations[cacheLastCleanupAnnotation] = time.Now().Add(-material.config.GCInterval.ToStdlib()).Format(time.RFC3339Nano)
	require.NoError(t, c.Update(ctx, &gcLease))

	// Maintenance wins after issuance has read the Lease but before its CAS update.
	maintenanceServer := &ComponentServer{Client: c, IdentityIndex: identityIndex, ManagementNamespace: namespace}
	raced := false
	server.Client = interceptor.NewClient(c, interceptor.Funcs{
		Update: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == gcLease.Name && !raced {
				raced = true
				grant, err = maintenanceServer.beginCacheGC(cacheContext, identity, material, sessionID)
				require.NoError(t, err)
			}
			return underlying.Update(ctx, obj, opts...)
		},
	})
	runtimeCache, err = server.runtimeCache(ctx, cs, operation)
	require.NoError(t, err)
	require.True(t, raced)
	require.Nil(t, runtimeCache)
	server.Client = c

	// An expired grant releases issuance without a cleanup callback.
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&gcLease), &gcLease))
	gcLease.Spec.RenewTime, gcLease.Spec.LeaseDurationSeconds = &expired, ptr.To(int32(1))
	require.NoError(t, c.Update(ctx, &gcLease))
	runtimeCache, err = server.runtimeCache(ctx, cs, operation)
	require.NoError(t, err)
	require.NotNil(t, runtimeCache)

	wrongContext := contextWithComponent(ctx, componentIdentity{Role: "cache", UID: "other-cache"})
	_, _, err = server.cacheForRequest(wrongContext)
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&ownerLease), &ownerLease))
	ownerLease.Spec.RenewTime = &expired
	require.NoError(t, c.Update(ctx, &ownerLease))
	runtimeCache, err = server.runtimeCache(ctx, cs, operation)
	require.NoError(t, err)
	require.Nil(t, runtimeCache)
	ownerLease.Labels[ComponentUIDLabel] = "deleted-component"
	ownerLease.Spec.HolderIdentity = ptr.To("deleted/component/owner/1")
	require.NoError(t, c.Update(ctx, &ownerLease))
	err = server.claimCache(cacheContext, identity, material, sessionID)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&ownerLease), &ownerLease))
	require.Equal(t, string(component.UID), ownerLease.Labels[ComponentUIDLabel])
	require.True(t, metav1.IsControlledBy(&ownerLease, component))
	ownerLease.Spec.RenewTime = &expired
	require.NoError(t, c.Update(ctx, &ownerLease))
	rpcServer := connect.NewServer()
	componentv1connect.RegisterComponentServiceHandler(rpcServer, server)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, rpcServer)
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(contextWithComponent(r.Context(), identity)))
	}))
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	defer httpServer.Close()
	remote := componentv1connect.NewComponentServiceClient(connect.NewClient(connecthttp.NewTransport(httpServer.Client(), httpServer.URL, connecthttp.WithGRPC())))
	stream, err := remote.CacheControl(ctx)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	require.NoError(t, stream.Send(&componentv1.CacheControlRequest{SessionId: "stream", Sequence: 1}))
	response, err := stream.Receive()
	require.NoError(t, err)
	require.Positive(t, response.PermitValidForMilliseconds)
	require.NoError(t, stream.CloseSend())
	_, err = stream.Receive()
	require.ErrorIs(t, err, io.EOF)
	err = server.claimCache(cacheContext, identity, material, "replacement")
	require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
}
