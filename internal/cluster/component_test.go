// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/accessticket"
	cachepkg "gitea.dev/codespace/internal/cache"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	configpkg "gitea.dev/codespace/internal/config"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type openAuthorizationRemote struct {
	codespacev1connect.UnimplementedManagerServiceHandler
	calls   int
	binding *codespacev1.OpenTokenBinding
}

func (r *openAuthorizationRemote) ValidateOpenToken(context.Context, *connect.Request[codespacev1.ValidateOpenTokenRequest]) (*connect.Response[codespacev1.ValidateOpenTokenResponse], error) {
	r.calls++
	if r.binding == nil {
		return connect.NewResponse(&codespacev1.ValidateOpenTokenResponse{Outcome: &codespacev1.ValidateOpenTokenResponse_Denied{Denied: &codespacev1.FailureDetail{Category: "invalid_code"}}}), nil
	}
	return connect.NewResponse(&codespacev1.ValidateOpenTokenResponse{Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{Allowed: r.binding}}), nil
}

func serveManagerRemote(t *testing.T, remote codespacev1connect.ManagerServiceHandler) *httptest.Server {
	t.Helper()
	path, handler := codespacev1connect.NewManagerServiceHandler(remote)
	mux := http.NewServeMux()
	mux.Handle("/api/codespace"+path, http.StripPrefix("/api/codespace", handler))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestGatewayOpenCodeUsesOnlyTargetRuntimeSite(t *testing.T) {
	const runtimeUUID = "933ef4a9-54c7-4f36-aad9-32ea72c4c986"
	target := &openAuthorizationRemote{binding: &codespacev1.OpenTokenBinding{UserId: 1, RuntimeUuid: runtimeUUID, EndpointId: "workspace", InteractionGeneration: 3}}
	unrelated := &openAuthorizationRemote{}
	targetServer, unrelatedServer := serveManagerRemote(t, target), serveManagerRemote(t, unrelated)

	site := testSite("target", "target-site")
	site.Status.CanonicalURL = targetServer.URL
	site.Spec.Credential = api.ResourceReference{Name: "target-credential", UID: "target-credential-uid"}
	unrelatedSite := testSite("unrelated", "unrelated-site")
	unrelatedSite.Status.CanonicalURL = unrelatedServer.URL
	unrelatedSite.Spec.Credential = api.ResourceReference{Name: "unrelated-credential", UID: "unrelated-credential-uid"}
	credential := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: site.Spec.Credential.Name, Namespace: "codespace-system", UID: site.Spec.Credential.UID, Labels: map[string]string{SiteUIDLabel: string(site.UID)}}, Data: map[string][]byte{"managerSecret": []byte("target")}}
	unrelatedCredential := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: unrelatedSite.Spec.Credential.Name, Namespace: "codespace-system", UID: unrelatedSite.Spec.Credential.UID, Labels: map[string]string{SiteUIDLabel: string(unrelatedSite.UID)}}, Data: map[string][]byte{"managerSecret": []byte("unrelated")}}
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-target", UID: "runtime-uid"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}, RuntimeUUID: runtimeUUID}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(site, unrelatedSite, credential, unrelatedCredential, cs).Build()
	index := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cs).WithIndex(&api.Codespace{}, RuntimeUUIDIndex, func(object client.Object) []string {
		return []string{object.(*api.Codespace).Spec.RuntimeUUID}
	}).Build()
	tracker := &RuntimeActivityTracker{}
	server := &ComponentServer{Client: c, IdentityIndex: index, ManagementNamespace: "codespace-system", Activity: tracker}
	ctx := contextWithComponent(t.Context(), componentIdentity{Role: "gateway", UID: "gateway-uid", Instance: "gateway-pod"})

	response, err := server.AuthorizeGateway(ctx, connect.NewRequest(&componentv1.AuthorizeGatewayRequest{ProtocolVersion: 1, Request: &componentv1.AuthorizeGatewayRequest_OpenCode{OpenCode: &componentv1.OpenCodeAuthorization{Code: "secret", RuntimeUuid: runtimeUUID, EndpointId: "workspace"}}}))
	require.NoError(t, err)
	require.True(t, response.Msg.Allowed)
	require.Equal(t, 1, target.calls)
	require.Zero(t, unrelated.calls)

	target.binding.EndpointId = "other"
	response, err = server.AuthorizeGateway(ctx, connect.NewRequest(&componentv1.AuthorizeGatewayRequest{ProtocolVersion: 1, Request: &componentv1.AuthorizeGatewayRequest_OpenCode{OpenCode: &componentv1.OpenCodeAuthorization{Code: "secret", RuntimeUuid: runtimeUUID, EndpointId: "workspace"}}}))
	require.NoError(t, err)
	require.False(t, response.Msg.Allowed)
	require.Equal(t, "target_mismatch", response.Msg.DeniedCategory)
	require.Zero(t, unrelated.calls)
}

func TestGatewaySnapshotAndAccessTicketUseCurrentPodIdentity(t *testing.T) {
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	encoded, err := x509.MarshalPKCS8PrivateKey(hostPrivate)
	require.NoError(t, err)
	hostKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	component := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "codespace-system", UID: "gateway-uid", Labels: map[string]string{ComponentLabel: "gateway"}}, Data: map[string]string{
		"url": "https://workspace.example", "sshAddress": "workspace.example:2222", "sshHostKeySecret": "gateway-key", "sshHostKeySecretUID": "gateway-key-uid",
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-key", Namespace: component.Namespace, UID: "gateway-key-uid", Labels: map[string]string{ComponentUIDLabel: string(component.UID)}}, Data: map[string][]byte{"hostKey": hostKey}}
	site := testSite("example", "site-uid")
	site.Status.CanonicalURL = "https://gitea.example"
	site.Status.Conditions = []metav1.Condition{{Type: "InfrastructureReady", Status: metav1.ConditionTrue, ObservedGeneration: site.Generation}}
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "resource-uid"},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}, RuntimeUUID: "933ef4a9-54c7-4f36-aad9-32ea72c4c986"},
		Status: api.CodespaceStatus{
			Pod:    api.ObservedResource{Name: "runtime-pod", UID: "pod-uid"},
			Target: &api.AgentTarget{Version: 4, Ready: true, PrimaryContainerID: "container", Endpoints: []api.AgentEndpoint{{ID: "workspace", Label: "Workspace", ContainerID: "container", Port: 13337}}},
		},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cs.Status.Pod.Name, Namespace: cs.Namespace, UID: cs.Status.Pod.UID, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cs, api.GroupVersion.WithKind("Codespace"))}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.2.3", PodIPs: []corev1.PodIP{{IP: "10.1.2.3"}}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, secret, site, cs, pod).Build()
	index := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, secret, site, cs, pod).
		WithIndex(&api.Codespace{}, RuntimeUUIDIndex, func(object client.Object) []string { return []string{object.(*api.Codespace).Spec.RuntimeUUID} }).Build()
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	server := &ComponentServer{Client: c, IdentityIndex: index, ManagementNamespace: component.Namespace, Tickets: &AccessTickets{privateKey: signingKey}}
	ctx := contextWithComponent(t.Context(), componentIdentity{Role: "gateway", UID: string(component.UID), Instance: "gateway-pod"})
	snapshot, err := server.gatewaySnapshot(ctx, componentIdentity{Role: "gateway", UID: string(component.UID), Instance: "gateway-pod"})
	require.NoError(t, err)
	require.Len(t, snapshot.Runtimes, 1)
	route := snapshot.Runtimes[0]
	require.Equal(t, "10.1.2.3:8444", route.AgentAddress)
	require.Equal(t, "pod-uid", route.PodUid)
	require.EqualValues(t, 4, route.TargetVersion)
	require.NotEmpty(t, snapshot.Cursor)

	issued, err := server.IssueAgentAccess(ctx, connect.NewRequest(&componentv1.IssueAgentAccessRequest{ProtocolVersion: 1, RuntimeUuid: cs.Spec.RuntimeUUID, Capability: agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT, EndpointId: "workspace"}))
	require.NoError(t, err)
	claims, err := accessticket.Verify(signingKey.Public().(ed25519.PublicKey), issued.Msg.Ticket, time.Now())
	require.NoError(t, err)
	require.Equal(t, string(cs.UID), claims.ResourceUid)
	require.Equal(t, string(pod.UID), claims.PodUid)
	require.EqualValues(t, cs.Status.Target.Version, claims.TargetVersion)

	require.NoError(t, c.Delete(t.Context(), component))
	component.ResourceVersion, component.UID = "", types.UID("replacement")
	require.NoError(t, c.Create(t.Context(), component))
	_, err = server.gatewaySnapshot(ctx, componentIdentity{Role: "gateway", UID: string(component.UID), Instance: "gateway-pod"})
	require.Error(t, err)
}

func contextWithComponent(ctx context.Context, identity componentIdentity) context.Context {
	return context.WithValue(ctx, componentIdentityContextKey{}, identity)
}

func TestCacheControlAndRuntimeCredential(t *testing.T) {
	ctx := t.Context()
	namespace := "codespace-system"
	cacheConfig := configpkg.CacheConfig{
		Name: "Build cache", Revision: 7, Enabled: true, Listen: ":5000", PublicURL: "https://cache.example.com", MaxSize: "1GiB",
		Storage:   configpkg.CacheStorageConfig{Driver: "filesystem", Path: "/var/lib/cache"},
		Upstreams: map[string]configpkg.RuntimeCacheUpstreamConfig{"ghcr.io": {Allow: []string{"devcontainers/*"}}},
	}
	encoded, err := json.Marshal(cacheConfig)
	require.NoError(t, err)
	component := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: namespace, UID: "cache-uid", Generation: 3, Labels: map[string]string{ComponentLabel: "cache"}}, Data: map[string]string{
		"config": string(encoded), "secretName": "cache-key", "secretUID": "cache-key-uid",
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cache-key", Namespace: namespace, UID: "cache-key-uid", Labels: map[string]string{ComponentUIDLabel: string(component.UID)}}, Data: map[string][]byte{
		"registryKey": []byte(strings.Repeat("r", 32)), "tokenKey": []byte(strings.Repeat("t", 32)),
	}}
	site := testSite("example", "site-uid")
	site.Spec.Caches = []api.ResourceReference{{Name: "offline-cache", UID: "offline-cache-uid"}, {Name: component.Name, UID: component.UID}}
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"}, Spec: api.CodespaceSpec{
		Site: api.ResourceReference{Name: site.Name, UID: site.UID}, Runtime: testRuntime(),
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, secret, site, cs).Build()
	server := &ComponentServer{Client: c, ManagementNamespace: namespace}
	cacheContext := contextWithComponent(ctx, componentIdentity{Role: "cache", UID: string(component.UID), Instance: "cache-pod"})

	identity, material, err := server.cacheForRequest(cacheContext, component.Name)
	require.NoError(t, err)
	require.EqualValues(t, cacheConfig.Revision, material.config.Revision)

	const sessionID = "11111111-1111-4111-8111-111111111111"
	_, err = server.claimCache(cacheContext, identity, material, sessionID)
	require.NoError(t, err)
	var ownerLease coordinationv1.Lease
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "cache-owner-" + component.Name}, &ownerLease))
	require.True(t, metav1.IsControlledBy(&ownerLease, component))
	_, err = server.claimCache(cacheContext, identity, material, "22222222-2222-4222-8222-222222222222")
	require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
	err = server.renewCache(cacheContext, identity, material, sessionID, &componentv1.CacheStatus{CacheBytes: 10, MirrorBytes: 20, CleanupResult: "ok"})
	require.NoError(t, err)

	operation := &codespacev1.OperationPayload{Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{
		Repository: &codespacev1.RepositoryCheckout{RepositoryId: 7}, GitIdentity: &codespacev1.GitIdentity{UserId: 9},
	}}}
	runtimeCache, err := server.runtimeCache(ctx, cs, operation)
	require.NoError(t, err)
	cacheNamespace := cachepkg.Namespace(secret.Data["registryKey"], site.Spec.ManagerID, 7, 9)
	require.Equal(t, "http://cache.codespace-system.svc:5000/cache/"+cacheNamespace, runtimeCache.BuildRegistry)
	credential := runtimeCache.Credentials["cache.codespace-system.svc:5000"]
	require.NotNil(t, credential)
	require.Equal(t, cachepkg.Username, credential.Username)
	verified, err := cachepkg.VerifyCredential(secret.Data["tokenKey"], component.Name, credential.Password, time.Now())
	require.NoError(t, err)
	require.Equal(t, cacheNamespace, verified.Namespace)
	require.True(t, verified.Policy.Build)
	grant, err := server.beginCacheGC(cacheContext, identity, material, sessionID)
	require.NoError(t, err)
	var gcLease coordinationv1.Lease
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "cache-gc-" + component.Name}, &gcLease))
	require.True(t, metav1.IsControlledBy(&gcLease, component))
	err = server.completeCacheGC(cacheContext, identity, component.Name, sessionID, grant.Id)
	require.NoError(t, err)

	wrongContext := contextWithComponent(ctx, componentIdentity{Role: "cache", UID: "other-cache", Instance: "cache-pod"})
	_, _, err = server.cacheForRequest(wrongContext, component.Name)
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	server.releaseCache(cacheContext, identity, component.Name, sessionID)
	runtimeCache, err = server.runtimeCache(ctx, cs, operation)
	require.NoError(t, err)
	require.Nil(t, runtimeCache)
	expired := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	ownerLease = coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "cache-owner-" + component.Name, Namespace: namespace, Labels: map[string]string{ComponentUIDLabel: "deleted-component"}}, Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To("deleted/component/owner/1"), LeaseDurationSeconds: ptr.To(int32(1)), RenewTime: &expired}}
	require.NoError(t, c.Create(ctx, &ownerLease))
	_, err = server.claimCache(cacheContext, identity, material, sessionID)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&ownerLease), &ownerLease))
	require.Equal(t, string(component.UID), ownerLease.Labels[ComponentUIDLabel])
	require.True(t, metav1.IsControlledBy(&ownerLease, component))
	server.releaseCache(cacheContext, identity, component.Name, sessionID)
}
