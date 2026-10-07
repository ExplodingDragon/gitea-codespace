// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	"gitea.dev/codespace-proto-go/agent/v1/agentv1connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAgentResultReplay(t *testing.T) {
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-test", UID: "runtime-uid"}, Spec: api.CodespaceSpec{Operation: api.Operation{Type: "create", Version: 1}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(cs).Build()
	server := &AgentControlServer{Client: c}
	report := &agentv1.AgentReport{
		OperationRversion: 1,
		Result:            &agentv1.OperationResult{OperationRversion: 1, Status: codespacev1.FinalStatus_FINAL_STATUS_DONE},
		Boot:              &codespacev1.RuntimeBoot{OperationRversion: 1, Stage: codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY, StartedUnix: 1, LastUpdateUnix: 2},
		Target:            &agentv1.AccessTarget{Version: 1, Ready: true, PrimaryContainerId: "container", Endpoints: []*agentv1.EndpointTarget{{Endpoint: &codespacev1.RuntimeEndpoint{EndpointId: "workspace", Label: "Workspace"}, ContainerId: "container", Port: 13337}}},
	}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.NoError(t, server.saveReport(t.Context(), cs, report))
	cs.Status.SettledOperationVersion = 1
	require.NoError(t, c.Status().Update(t.Context(), cs))
	require.NoError(t, server.saveReport(t.Context(), cs, report))
	require.EqualValues(t, 1, cs.Status.SettledOperationVersion)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	report.Target.Endpoints[0].Port++
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(server.saveReport(t.Context(), cs, report)))
	report.Target.Endpoints[0].Port--
	report.Result.Status = codespacev1.FinalStatus_FINAL_STATUS_FAILED
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(server.saveReport(t.Context(), cs, report)))
}

func TestAgentControlRecovery(t *testing.T) {
	site := testSite("example", "site-uid")
	site.Status.NamespaceUID = "namespace-uid"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "codespace-example", UID: "namespace-uid", Labels: map[string]string{SiteUIDLabel: string(site.UID)}}}
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: ns.Name, UID: "runtime-uid"},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}, Operation: api.Operation{Type: "stop", Version: 3}},
		Status:     api.CodespaceStatus{Bound: true, Pod: api.ObservedResource{Name: "runtime", UID: "pod-uid"}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cs.Status.Pod.Name, Namespace: ns.Name, UID: cs.Status.Pod.UID, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cs, api.GroupVersion.WithKind("Codespace"))}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(site, ns, cs, pod).
		WithIndex(&api.GiteaSite{}, IdentityUIDIndex, func(object client.Object) []string { return []string{string(object.GetUID())} }).
		WithIndex(&api.Codespace{}, IdentityUIDIndex, func(object client.Object) []string { return []string{string(object.GetUID())} }).Build()
	server := &AgentControlServer{Client: c, IdentityIndex: c}
	_, handler := server.Handler()
	identity, err := url.Parse("spiffe://codespace/agent/site-uid/runtime-uid/pod-uid")
	require.NoError(t, err)
	certificate := &x509.Certificate{URIs: []*url.URL{identity}}
	open := func() (<-chan struct{}, *io.PipeWriter) {
		reader, writer := io.Pipe()
		req := httptest.NewRequest(http.MethodPost, agentv1connect.AgentControlServiceControlProcedure, reader)
		req.ProtoMajor, req.ProtoMinor = 2, 0
		req.Header.Set("Content-Type", "application/connect+proto")
		req.Header.Set("Connect-Protocol-Version", "1")
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}, VerifiedChains: [][]*x509.Certificate{{certificate}}}
		done := make(chan struct{})
		go func() {
			defer close(done)
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
		t.Cleanup(func() { _ = writer.Close() })
		return done, writer
	}
	firstDone, _ := open()
	require.Eventually(t, func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		return server.sessions[cs.UID] != nil
	}, time.Second, time.Millisecond)
	secondDone, writer := open()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("superseded control stream remained blocked in Receive")
	}
	server.mu.Lock()
	active := len(server.sessions)
	server.mu.Unlock()
	require.Equal(t, 1, active)
	require.NoError(t, writer.Close())
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("closed control stream did not release its session")
	}
	server.mu.Lock()
	active = len(server.sessions)
	server.mu.Unlock()
	require.Zero(t, active)

	// The TLS verifier has separate coverage; this fixture supplies its verified
	// identity to exercise result recovery over a real HTTP/2 control stream.
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}, VerifiedChains: [][]*x509.Certificate{{certificate}}}
		handler.ServeHTTP(w, r)
	}))
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	defer httpServer.Close()
	remote := agentv1connect.NewAgentControlServiceClient(httpServer.Client(), httpServer.URL, connect.WithGRPC())
	stream := remote.Control(t.Context())
	defer func() { require.NoError(t, stream.CloseResponse()) }()
	require.NoError(t, stream.Send(&agentv1.ControlRequest{
		ProtocolVersion: 1, SessionId: "reconnected", Sequence: 1,
		Report: &agentv1.AgentReport{OperationRversion: 3, Result: &agentv1.OperationResult{OperationRversion: 3, Status: codespacev1.FinalStatus_FINAL_STATUS_DONE}},
	}))
	response, err := stream.Receive()
	require.NoError(t, err)
	require.True(t, response.CancelExecution)
	require.EqualValues(t, 3, response.AcknowledgedResultVersion)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.NotNil(t, cs.Status.Result)
	require.True(t, cs.Status.Result.Succeeded)
	require.NoError(t, stream.CloseRequest())

	access := &runtimeAccessTestServer{t: t}
	giteaMux := http.NewServeMux()
	path, giteaHandler := codespacev1connect.NewManagerServiceHandler(access)
	giteaMux.Handle("/api/codespace"+path, http.StripPrefix("/api/codespace", giteaHandler))
	gitea := httptest.NewServer(giteaMux)
	defer gitea.Close()
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(site), site))
	site.Status.CanonicalURL = gitea.URL
	// Site status is not registered separately in this fixture.
	require.NoError(t, c.Update(t.Context(), site))
	credential := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: site.Spec.Credential.Name, Namespace: "codespace-system", UID: site.Spec.Credential.UID, Labels: map[string]string{SiteUIDLabel: string(site.UID)}}, Data: map[string][]byte{"managerSecret": []byte("test-manager-secret")}}
	require.NoError(t, c.Create(t.Context(), credential))
	server.ManagementNamespace = credential.Namespace
	cs.Spec.Operation.Type, cs.Spec.Operation.Version = "create", 4
	cs.Spec.CodespaceID, cs.Spec.RuntimeUUID = 1, "933ef4a9-54c7-4f36-aad9-32ea72c4c986"
	cs.Spec.Runtime = testRuntime()
	cs.Spec.Runtime.GitSSHKeyType = "rsa-4096"
	cs.Spec.Operation.Payload.Raw, err = protojson.Marshal(&codespacev1.OperationPayload{OperationRversion: 4, CodespaceId: cs.Spec.CodespaceID, RuntimeUuid: cs.Spec.RuntimeUUID, Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{}}})
	require.NoError(t, err)
	require.NoError(t, c.Update(t.Context(), cs))
	server.Authority.Grant(cs.UID, 4, time.Now().Add(time.Minute))
	startup := remote.Control(t.Context())
	defer func() { require.NoError(t, startup.CloseResponse()) }()
	require.NoError(t, startup.Send(&agentv1.ControlRequest{ProtocolVersion: 1, SessionId: "new-pod", Sequence: 1, Report: &agentv1.AgentReport{OperationRversion: 3, Result: &agentv1.OperationResult{OperationRversion: 3, Status: codespacev1.FinalStatus_FINAL_STATUS_DONE}}}))
	response, err = startup.Receive()
	require.NoError(t, err)
	require.Equal(t, "rsa-4096", response.Runtime.GitSshKeyType)
	require.EqualValues(t, 4, response.Operation.OperationRversion)
	require.Zero(t, response.AcknowledgedResultVersion)
	require.True(t, response.CancelExecution)
	require.Nil(t, response.Access)
	require.NoError(t, startup.Send(&agentv1.ControlRequest{ProtocolVersion: 1, SessionId: "new-pod", Sequence: 2, AcceptedOperationRversion: 4, Report: &agentv1.AgentReport{OperationRversion: 4, GitSshPublicKey: []byte("public-key")}}))
	response, err = startup.Receive()
	require.NoError(t, err)
	require.EqualValues(t, 4, response.OperationRversion)
	require.Nil(t, response.Operation)
	require.Nil(t, response.Runtime)
	require.Equal(t, "runtime-token", response.Access.GiteaToken)
	require.Positive(t, response.PermitValidForMilliseconds)
	require.False(t, response.CancelExecution)
	require.NoError(t, startup.CloseRequest())
	for _, category := range []string{"ok", "log_size_exceeded", "stale_operation", "codespace_not_found", "offset_conflict", "unavailable"} {
		t.Run(category, func(t *testing.T) {
			reply, err := remote.UploadLogs(t.Context(), connect.NewRequest(&agentv1.UploadLogsRequest{ProtocolVersion: 1, OperationRversion: 4, Offset: 10, Lines: []*codespacev1.LogLine{{Message: category}}}))
			switch category {
			case "ok":
				require.NoError(t, err)
				require.EqualValues(t, 20, reply.Msg.NextOffset)
				require.False(t, reply.Msg.Closed)
			case "offset_conflict", "unavailable":
				require.Error(t, err)
			default:
				require.NoError(t, err)
				require.EqualValues(t, 10, reply.Msg.NextOffset)
				require.True(t, reply.Msg.Closed)
			}
		})
	}
}

type runtimeAccessTestServer struct {
	codespacev1connect.UnimplementedManagerServiceHandler
	t *testing.T
}

func (s *runtimeAccessTestServer) RequestRuntimeAccess(_ context.Context, request *connect.Request[codespacev1.RequestRuntimeAccessRequest]) (*connect.Response[codespacev1.RequestRuntimeAccessResponse], error) {
	require.Equal(s.t, "test-manager-secret", request.Header().Get("x-codespace-manager-secret"))
	require.Equal(s.t, []byte("public-key"), request.Msg.GitSshKey.PublicKey)
	return connect.NewResponse(&codespacev1.RequestRuntimeAccessResponse{Access: &codespacev1.RuntimeAccessBundle{GiteaToken: "runtime-token"}}), nil
}

func (s *runtimeAccessTestServer) UpdateLog(_ context.Context, request *connect.Request[codespacev1.UpdateLogRequest]) (*connect.Response[codespacev1.UpdateLogResponse], error) {
	category := request.Msg.Lines[0].Message
	if category == "ok" {
		return connect.NewResponse(&codespacev1.UpdateLogResponse{NextOffset: 20}), nil
	}
	err := connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s", category))
	detail, detailErr := connect.NewErrorDetail(&codespacev1.FailureDetail{Category: category})
	require.NoError(s.t, detailErr)
	err.AddDetail(detail)
	return nil, err
}

func TestAgentStartupRequiresCurrentPrivateWorkspace(t *testing.T) {
	cs := &api.Codespace{Spec: api.CodespaceSpec{Operation: api.Operation{Type: "resume", Version: 3}}}
	report := &agentv1.AgentReport{
		OperationRversion: 3,
		Result:            &agentv1.OperationResult{OperationRversion: 3, Status: codespacev1.FinalStatus_FINAL_STATUS_DONE},
		Boot:              &codespacev1.RuntimeBoot{OperationRversion: 2, Stage: codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY, StartedUnix: 1, LastUpdateUnix: 2},
		Target:            &agentv1.AccessTarget{Version: 3, Ready: true, PrimaryContainerId: "container", Endpoints: []*agentv1.EndpointTarget{{Endpoint: &codespacev1.RuntimeEndpoint{EndpointId: "workspace", Label: "Workspace", Public: true}, ContainerId: "container", Port: 13337}}},
	}
	server := &AgentControlServer{}
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(server.saveReport(t.Context(), cs, report)))
	report.Boot.OperationRversion = 3
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(server.saveReport(t.Context(), cs, report)))
	report.Target.Endpoints = nil
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(server.saveReport(t.Context(), cs, report)))
}

func TestExecutionAuthorityVersions(t *testing.T) {
	authority := &ExecutionAuthority{}
	require.Zero(t, authority.Remaining("runtime", 1))
	authority.Grant("runtime", 2, time.Now().Add(time.Minute))
	authority.Grant("runtime", 1, time.Now().Add(time.Hour))
	require.Zero(t, authority.Remaining("runtime", 1))
	require.Positive(t, authority.Remaining("runtime", 2))
	authority.Grant("runtime", 3, time.Now().Add(-time.Second))
	require.Zero(t, authority.Remaining("runtime", 2))
	require.Zero(t, authority.Remaining("runtime", 3))
}

func TestAgentProgressAndTargetRefresh(t *testing.T) {
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"}, Spec: api.CodespaceSpec{Operation: api.Operation{Type: "create", Version: 1}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(cs).Build()
	server := &AgentControlServer{Client: c}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	report := &agentv1.AgentReport{OperationRversion: 1, Boot: &codespacev1.RuntimeBoot{OperationRversion: 1, Stage: codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_RUNTIME, StartedUnix: 1, LastUpdateUnix: 2}}
	require.NoError(t, server.saveReport(t.Context(), cs, report))
	require.Nil(t, cs.Status.Result)
	version := cs.ResourceVersion
	require.NoError(t, server.saveReport(t.Context(), cs, report))
	require.Equal(t, version, cs.ResourceVersion)
	report.Boot.Stage, report.Boot.LastUpdateUnix = codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY, 3
	report.Target = &agentv1.AccessTarget{Version: 1, Ready: true, PrimaryContainerId: "container", Endpoints: []*agentv1.EndpointTarget{{Endpoint: &codespacev1.RuntimeEndpoint{EndpointId: "workspace", Label: "Workspace"}, ContainerId: "container", Port: 13337}}}
	report.Result = &agentv1.OperationResult{OperationRversion: 1, Status: codespacev1.FinalStatus_FINAL_STATUS_DONE}
	require.NoError(t, server.saveReport(t.Context(), cs, report))
	report.Result = nil
	report.Target.Version++
	report.Target.Endpoints = append(report.Target.Endpoints, &agentv1.EndpointTarget{Endpoint: &codespacev1.RuntimeEndpoint{EndpointId: "p8000", Label: "Preview", Public: true, Port: 8000}, ContainerId: "container", Port: 8000})
	require.NoError(t, server.saveReport(t.Context(), cs, report))
	require.Len(t, cs.Status.Target.Endpoints, 2)
	report.Target.Version++
	report.Target.Endpoints[1].Endpoint.Port = 9000
	require.Error(t, server.saveReport(t.Context(), cs, report))
	report.Target.Endpoints[1].Endpoint.Port = 8000
	for _, id := range []string{"trailing-", strings.Repeat("x", 31)} {
		report.Target.Endpoints[1].Endpoint.EndpointId = id
		require.Error(t, server.saveReport(t.Context(), cs, report))
	}
	report.Target = &agentv1.AccessTarget{Version: 3}
	require.NoError(t, server.saveReport(t.Context(), cs, report))
	require.False(t, cs.Status.Target.Ready)
	require.True(t, cs.Status.Result.Succeeded)
}
