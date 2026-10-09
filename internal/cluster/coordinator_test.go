// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/go-logr/logr/testr"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type coordinatorRemote struct {
	operationRemote
	mu           sync.Mutex
	declarations []codespacev1.ManagerRuntimeState
	claimed      bool
	finalized    bool
	stopping     bool
	runtimeUUID  string
	url          string
	inventory    func(*codespacev1.ReportInstancesRequest)
	idleStop     func(*codespacev1.RequestIdleStopRequest) *codespacev1.RequestIdleStopResponse
}

func (s *coordinatorRemote) RequestIdleStop(_ context.Context, request *connect.Request[codespacev1.RequestIdleStopRequest]) (*connect.Response[codespacev1.RequestIdleStopResponse], error) {
	return connect.NewResponse(s.idleStop(request.Msg)), nil
}

func (s *coordinatorRemote) DeclareManager(_ context.Context, request *connect.Request[codespacev1.DeclareManagerRequest]) (*connect.Response[codespacev1.DeclareManagerResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declarations = append(s.declarations, request.Msg.ManagerRuntimeState)
	return connect.NewResponse(&codespacev1.DeclareManagerResponse{HeartbeatIntervalMilliseconds: 10000, RuntimeMetadataRefreshIntervalMilliseconds: 1000, ControlPlaneMaxMessageSizeBytes: api.MaxObjectBytes, GiteaWebUrl: s.url}), nil
}

func (s *coordinatorRemote) ReportInstances(_ context.Context, request *connect.Request[codespacev1.ReportInstancesRequest]) (*connect.Response[codespacev1.ReportInstancesResponse], error) {
	if s.inventory != nil {
		s.inventory(request.Msg)
	}
	response := &codespacev1.ReportInstancesResponse{}
	for _, instance := range request.Msg.Instances {
		response.Results = append(response.Results, &codespacev1.RuntimeInstanceResult{RuntimeUuid: instance.RuntimeUuid})
	}
	return connect.NewResponse(response), nil
}

func TestInventoryMissingPodRetainsRuntime(t *testing.T) {
	site := testSite("example", "site-uid")
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"},
		Spec:       api.CodespaceSpec{RuntimeUUID: "933ef4a9-54c7-4f36-aad9-32ea72c4c986", Operation: api.Operation{Type: "create", Version: 1}},
		Status:     api.CodespaceStatus{SettledOperationVersion: 1, Pod: api.ObservedResource{Name: "missing-pod", UID: "pod-uid"}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(site, cs).WithObjects(site, cs).Build()
	coordinator := &SiteCoordinator{Operations: Operations{Client: c, Authority: &ExecutionAuthority{}}}
	remote := &coordinatorRemote{inventory: func(request *codespacev1.ReportInstancesRequest) {
		require.Len(t, request.Instances, 1)
		require.Equal(t, codespacev1.RuntimeState_RUNTIME_STATE_CREATING, request.Instances[0].RuntimeState)
	}}
	require.NoError(t, coordinator.inventory(t.Context(), site, []api.Codespace{*cs}, remote))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.Empty(t, cs.Status.RecoveryAction)
}

func TestCoordinatorRequestsIdleStopAfterObservedInactivity(t *testing.T) {
	site := testSite("example", "site-uid")
	tracker := &RuntimeActivityTracker{}
	now := time.Now()
	settings := &codespacev1.EffectiveCodespaceRuntimeSettings{AutoStopEnabled: true, IdleTimeoutSeconds: 1, InteractionGeneration: 2}
	require.NoError(t, tracker.ObserveSettings(string(site.UID), "runtime", settings, now.Add(-2*time.Second)))
	require.NoError(t, tracker.ReplaceGateway(string(site.Spec.Gateway.UID), "gateway-pod", nil, false, now.Add(-2*time.Second)))
	_, ready := tracker.IdleRequest(string(site.UID), "runtime", string(site.Spec.Gateway.UID), true, now.Add(-2*time.Second))
	require.False(t, ready)

	cs := api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", UID: "runtime-uid"}, Spec: api.CodespaceSpec{RuntimeUUID: "runtime", Operation: api.Operation{Version: 1}}, Status: api.CodespaceStatus{Bound: true, SettledOperationVersion: 1, Pod: api.ObservedResource{UID: "pod-uid"}, Target: &api.AgentTarget{Ready: true}}}
	requests := 0
	remote := &coordinatorRemote{idleStop: func(request *codespacev1.RequestIdleStopRequest) *codespacev1.RequestIdleStopResponse {
		requests++
		require.Equal(t, "runtime", request.RuntimeUuid)
		require.Equal(t, settings, request.ObservedSettings)
		return &codespacev1.RequestIdleStopResponse{Outcome: &codespacev1.RequestIdleStopResponse_Pending{Pending: &codespacev1.IdleStopPending{OperationRversion: 2}}}
	}}
	coordinator := &SiteCoordinator{Operations: Operations{Activity: tracker}}
	coordinator.requestIdleStops(t.Context(), site, []api.Codespace{cs}, remote)
	require.Equal(t, 1, requests)
	coordinator.requestIdleStops(t.Context(), site, []api.Codespace{cs}, remote)
	require.Equal(t, 1, requests)
}

func (s *coordinatorRemote) FetchOperations(_ context.Context, request *connect.Request[codespacev1.FetchOperationsRequest]) (*connect.Response[codespacev1.FetchOperationsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	response := &codespacev1.FetchOperationsResponse{}
	if !s.claimed && request.Msg.StartupCapacityAvailable > 0 {
		s.claimed = true
		response.Operations = []*codespacev1.OperationPayload{{CodespaceId: 1, OperationRversion: 1, LeaseValidForMilliseconds: 60000, Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{EnvironmentTag: "standard", RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{}}}}}
	}
	if s.finalized && !s.stopping && request.Msg.CleanupCapacityAvailable > 0 {
		s.stopping = true
		response.Operations = append(response.Operations, &codespacev1.OperationPayload{CodespaceId: 1, RuntimeUuid: s.runtimeUUID, OperationRversion: 2, LeaseValidForMilliseconds: 60000, Command: &codespacev1.OperationPayload_Stop{Stop: &codespacev1.StopOperationPayload{}}})
	}
	for _, observed := range request.Msg.ObservedOperations {
		response.RenewedLeases = append(response.RenewedLeases, &codespacev1.RenewedOperationLease{RuntimeUuid: observed.RuntimeUuid, OperationRversion: observed.OperationRversion, LeaseValidForMilliseconds: 60000})
	}
	return connect.NewResponse(response), nil
}

func TestSiteCoordinatorLifecycle(t *testing.T) {
	remote := &coordinatorRemote{}
	remote.bind = func(request *codespacev1.BindRuntimeIdentityRequest) (*codespacev1.BindRuntimeIdentityResponse, error) {
		remote.mu.Lock()
		remote.runtimeUUID = request.RuntimeUuid
		remote.mu.Unlock()
		return &codespacev1.BindRuntimeIdentityResponse{RuntimeUuid: request.RuntimeUuid}, nil
	}
	remote.metadata = func(*codespacev1.ReportRuntimeMetadataRequest) error { return nil }
	remote.final = func(request *codespacev1.FinalizeOperationRequest) (*codespacev1.FinalizeOperationResponse, error) {
		remote.mu.Lock()
		defer remote.mu.Unlock()
		remote.finalized = true
		return &codespacev1.FinalizeOperationResponse{}, nil
	}
	path, handler := codespacev1connect.NewManagerServiceHandler(remote)
	mux := http.NewServeMux()
	mux.Handle("/api/codespace"+path, http.StripPrefix("/api/codespace", handler))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-codespace-manager-id") != "1" || r.Header.Get("x-codespace-manager-secret") != "test-secret" {
			http.Error(w, "invalid site credential", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	remote.url = server.URL
	site := testSite("example", "site-uid")
	site.Status.NamespaceUID, site.Status.CanonicalURL = "namespace-uid", server.URL
	site.Status.Conditions = []metav1.Condition{{Type: "InfrastructureReady", Status: metav1.ConditionTrue, Reason: "IdentityVerified", ObservedGeneration: 1}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "codespace-example", UID: site.Status.NamespaceUID, Labels: map[string]string{SiteUIDLabel: string(site.UID)}}}
	template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "template-uid", Generation: 1}, Spec: api.EnvironmentTemplateSpec{Tag: "standard", Runtime: testEnvironment()}, Status: api.EnvironmentTemplateStatus{Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Verified", ObservedGeneration: 1}}}}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(private, "")
	require.NoError(t, err)
	gateway := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "codespace-system", UID: "gateway-uid", Labels: map[string]string{ComponentLabel: "gateway"}}, Data: map[string]string{"url": "https://workspace.example", "sshAddress": "workspace.example:2222", "sshHostKeySecret": "gateway-key", "sshHostKeySecretUID": "gateway-key-uid"}}
	hostKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-key", Namespace: gateway.Namespace, UID: "gateway-key-uid", CreationTimestamp: metav1.Now(), Labels: map[string]string{ComponentUIDLabel: string(gateway.UID)}}, Data: map[string][]byte{"hostKey": pem.EncodeToMemory(block)}}
	credential := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "site-credential", Namespace: gateway.Namespace, UID: "credential-uid", Labels: map[string]string{SiteUIDLabel: string(site.UID)}}, Data: map[string][]byte{"managerSecret": []byte("test-secret")}}
	quota := testQuota(site, ns.Name)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&api.Codespace{}, site).WithObjects(site, ns, template, gateway, hostKey, credential, quota).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			object.SetUID(types.UID(uuid.NewString()))
			return c.Create(ctx, object, opts...)
		}}).Build()
	coordinator := &SiteCoordinator{Operations: Operations{Client: c, Authority: &ExecutionAuthority{}, ManagementNamespace: gateway.Namespace, PlatformImage: testRuntime().Image}}
	ctx, cancel := context.WithCancel(t.Context())
	ctx = log.IntoContext(ctx, testr.New(t))
	done := make(chan struct{})
	go func() { defer close(done); coordinator.runSite(ctx, site.Name, site.UID) }()
	t.Cleanup(func() { cancel(); <-done })
	key := types.NamespacedName{Namespace: ns.Name, Name: "codespace-1"}
	var cs api.Codespace
	require.Eventually(t, func() bool {
		return c.Get(t.Context(), key, &cs) == nil && cs.Status.Bound && coordinator.Authority.Remaining(cs.UID, 1) > 0
	}, 5*time.Second, 10*time.Millisecond)
	control := &AgentControlServer{Client: c}
	require.NoError(t, control.saveReport(t.Context(), &cs, &agentv1.AgentReport{
		OperationRversion: 1, Result: &agentv1.OperationResult{OperationRversion: 1, Status: codespacev1.FinalStatus_FINAL_STATUS_DONE},
		Boot:   &codespacev1.RuntimeBoot{OperationRversion: 1, Stage: codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY, StartedUnix: 1, LastUpdateUnix: 2},
		Target: &agentv1.AccessTarget{Version: 1, Ready: true, PrimaryContainerId: "container", Endpoints: []*agentv1.EndpointTarget{{Endpoint: &codespacev1.RuntimeEndpoint{EndpointId: "workspace", Label: "Workspace"}, ContainerId: "container", Port: 13337}}},
	}))
	require.Eventually(t, func() bool {
		return c.Get(t.Context(), key, &cs) == nil && cs.Spec.Operation.Version == 2
	}, 5*time.Second, 10*time.Millisecond)
	runtime := &RuntimeReconciler{Client: c, ManagementNamespace: gateway.Namespace, Authority: coordinator.Authority}
	_, err = runtime.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, c.Get(t.Context(), key, &cs))
	require.True(t, meta.IsStatusConditionTrue(cs.Status.Conditions, "Stopped"), "%+v", cs.Status)
	require.Eventually(t, func() bool {
		return c.Get(t.Context(), key, &cs) == nil && cs.Status.SettledOperationVersion == 2
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	require.Zero(t, coordinator.Authority.Remaining(cs.UID, 2))
	remote.mu.Lock()
	defer remote.mu.Unlock()
	require.GreaterOrEqual(t, len(remote.declarations), 2)
	require.Equal(t, codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_RECOVERING, remote.declarations[0])
	require.Equal(t, codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE, remote.declarations[1])
}
