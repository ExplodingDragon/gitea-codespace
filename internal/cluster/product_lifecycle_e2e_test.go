// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	configpkg "gitea.dev/codespace/internal/config"
	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type lifecycleRemote struct {
	codespacev1connect.UnimplementedManagerServiceHandler

	mu              sync.Mutex
	baseURL         string
	runtimeUUID     string
	operation       *codespacev1.OperationPayload
	delivered       bool
	lastFinalized   int64
	logOffsets      map[int64]int64
	logs            map[int64][]string
	metadata        *codespacev1.RuntimeMetadata
	runtimeSettings struct {
		autoStopEnabled       bool
		idleTimeoutSeconds    int64
		interactionGeneration int64
	}
	interactionGeneration int64
	finalized             chan *codespacev1.FinalizeOperationRequest
	managerOnline         chan struct{}
	onlineObserved        bool
	sshPublicKey          []byte
}

func newLifecycleRemote() *lifecycleRemote {
	return &lifecycleRemote{
		logOffsets:    make(map[int64]int64),
		logs:          make(map[int64][]string),
		finalized:     make(chan *codespacev1.FinalizeOperationRequest, 8),
		managerOnline: make(chan struct{}),
	}
}

func (s *lifecycleRemote) setOperation(operation *codespacev1.OperationPayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if operation.RuntimeUuid == "" {
		operation.RuntimeUuid = s.runtimeUUID
	}
	s.operation = operation
	s.delivered = false
	if create := operation.GetCreate(); create != nil && create.RuntimeSettings != nil {
		s.runtimeSettings.autoStopEnabled = create.RuntimeSettings.AutoStopEnabled
		s.runtimeSettings.idleTimeoutSeconds = create.RuntimeSettings.IdleTimeoutSeconds
		s.runtimeSettings.interactionGeneration = create.RuntimeSettings.InteractionGeneration
		s.interactionGeneration = create.RuntimeSettings.InteractionGeneration
	}
	if resume := operation.GetResume(); resume != nil && resume.RuntimeSettings != nil {
		s.runtimeSettings.autoStopEnabled = resume.RuntimeSettings.AutoStopEnabled
		s.runtimeSettings.idleTimeoutSeconds = resume.RuntimeSettings.IdleTimeoutSeconds
		s.runtimeSettings.interactionGeneration = resume.RuntimeSettings.InteractionGeneration
		s.interactionGeneration = resume.RuntimeSettings.InteractionGeneration
	}
}

func (s *lifecycleRemote) setRuntimeSettings(settings *codespacev1.EffectiveCodespaceRuntimeSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtimeSettings.autoStopEnabled = settings.AutoStopEnabled
	s.runtimeSettings.idleTimeoutSeconds = settings.IdleTimeoutSeconds
	s.runtimeSettings.interactionGeneration = settings.InteractionGeneration
	s.interactionGeneration = settings.InteractionGeneration
}

func (s *lifecycleRemote) CheckManager(_ context.Context, request *connect.Request[codespacev1.CheckManagerRequest]) (*connect.Response[codespacev1.CheckManagerResponse], error) {
	if request.Msg.ProtocolVersion != 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unsupported protocol"))
	}
	return connect.NewResponse(&codespacev1.CheckManagerResponse{GiteaWebUrl: s.baseURL, ManagerName: "Kubernetes lifecycle E2E"}), nil
}

func (s *lifecycleRemote) DeclareManager(_ context.Context, request *connect.Request[codespacev1.DeclareManagerRequest]) (*connect.Response[codespacev1.DeclareManagerResponse], error) {
	if request.Msg.ProtocolVersion != 1 || len(request.Msg.Environments) != 1 || request.Msg.Environments[0].Tag != "standard" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid declaration"))
	}
	s.mu.Lock()
	if request.Msg.ManagerRuntimeState == codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE && !s.onlineObserved {
		s.onlineObserved = true
		close(s.managerOnline)
	}
	s.mu.Unlock()
	return connect.NewResponse(&codespacev1.DeclareManagerResponse{
		HeartbeatIntervalMilliseconds:              1000,
		RuntimeMetadataRefreshIntervalMilliseconds: 1000,
		ControlPlaneMaxMessageSizeBytes:            api.MaxObjectBytes,
		GiteaWebUrl:                                s.baseURL,
	}), nil
}

func (s *lifecycleRemote) FetchOperations(_ context.Context, request *connect.Request[codespacev1.FetchOperationsRequest]) (*connect.Response[codespacev1.FetchOperationsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	response := &codespacev1.FetchOperationsResponse{}
	for _, observed := range request.Msg.ObservedOperations {
		if s.operation != nil && observed.RuntimeUuid == s.operation.RuntimeUuid && observed.OperationRversion == s.operation.OperationRversion {
			response.RenewedLeases = append(response.RenewedLeases, &codespacev1.RenewedOperationLease{
				RuntimeUuid: observed.RuntimeUuid, OperationRversion: observed.OperationRversion, LeaseValidForMilliseconds: 60000,
			})
		}
	}
	if s.operation == nil || s.delivered {
		return connect.NewResponse(response), nil
	}
	startup := s.operation.GetCreate() != nil || s.operation.GetResume() != nil
	if startup && request.Msg.StartupCapacityAvailable <= 0 || !startup && request.Msg.CleanupCapacityAvailable <= 0 {
		return connect.NewResponse(response), nil
	}
	if create := s.operation.GetCreate(); create != nil {
		accepted := false
		for _, tag := range request.Msg.AcceptedCreateTags {
			accepted = accepted || tag == create.EnvironmentTag
		}
		if !accepted {
			return connect.NewResponse(response), nil
		}
	}
	response.Operations = []*codespacev1.OperationPayload{s.operation}
	s.delivered = true
	return connect.NewResponse(response), nil
}

func (s *lifecycleRemote) BindRuntimeIdentity(_ context.Context, request *connect.Request[codespacev1.BindRuntimeIdentityRequest]) (*connect.Response[codespacev1.BindRuntimeIdentityResponse], error) {
	if request.Msg.ProtocolVersion != 1 || request.Msg.CodespaceId != 1 || request.Msg.OperationRversion != 1 || request.Msg.RuntimeUuid == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid runtime binding"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtimeUUID != "" && s.runtimeUUID != request.Msg.RuntimeUuid {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("runtime identity changed"))
	}
	s.runtimeUUID = request.Msg.RuntimeUuid
	if s.operation != nil {
		s.operation.RuntimeUuid = request.Msg.RuntimeUuid
	}
	return connect.NewResponse(&codespacev1.BindRuntimeIdentityResponse{RuntimeUuid: request.Msg.RuntimeUuid}), nil
}

func (s *lifecycleRemote) FinalizeOperation(_ context.Context, request *connect.Request[codespacev1.FinalizeOperationRequest]) (*connect.Response[codespacev1.FinalizeOperationResponse], error) {
	s.mu.Lock()
	if s.operation == nil || request.Msg.RuntimeUuid != s.runtimeUUID || request.Msg.OperationRversion != s.operation.OperationRversion || request.Msg.OperationRversion <= s.lastFinalized {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("operation identity changed"))
	}
	s.lastFinalized = request.Msg.OperationRversion
	s.operation = nil
	s.delivered = false
	s.mu.Unlock()
	s.finalized <- request.Msg
	return connect.NewResponse(&codespacev1.FinalizeOperationResponse{}), nil
}

func (s *lifecycleRemote) UpdateLog(_ context.Context, request *connect.Request[codespacev1.UpdateLogRequest]) (*connect.Response[codespacev1.UpdateLogResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Msg.ProtocolVersion != 1 || request.Msg.RuntimeUuid != s.runtimeUUID || request.Msg.Offset != s.logOffsets[request.Msg.OperationRversion] || len(request.Msg.Lines) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("log offset changed"))
	}
	s.logOffsets[request.Msg.OperationRversion] += int64(len(request.Msg.Lines))
	for _, line := range request.Msg.Lines {
		s.logs[request.Msg.OperationRversion] = append(s.logs[request.Msg.OperationRversion], line.Message)
	}
	return connect.NewResponse(&codespacev1.UpdateLogResponse{NextOffset: s.logOffsets[request.Msg.OperationRversion]}), nil
}

func (s *lifecycleRemote) ReportRuntimeMetadata(_ context.Context, request *connect.Request[codespacev1.ReportRuntimeMetadataRequest]) (*connect.Response[codespacev1.ReportRuntimeMetadataResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Msg.ProtocolVersion != 1 || request.Msg.RuntimeUuid != s.runtimeUUID || request.Msg.MetadataGeneration <= 0 || request.Msg.Metadata == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid runtime metadata"))
	}
	s.metadata = request.Msg.Metadata
	return connect.NewResponse(&codespacev1.ReportRuntimeMetadataResponse{}), nil
}

func (s *lifecycleRemote) RequestRuntimeAccess(_ context.Context, request *connect.Request[codespacev1.RequestRuntimeAccessRequest]) (*connect.Response[codespacev1.RequestRuntimeAccessResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Msg.ProtocolVersion != 1 || request.Msg.RuntimeUuid != s.runtimeUUID || request.Msg.OperationRversion <= 0 || len(request.Msg.GetGitSshKey().GetPublicKey()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid runtime access request"))
	}
	return connect.NewResponse(&codespacev1.RequestRuntimeAccessResponse{Access: &codespacev1.RuntimeAccessBundle{
		GiteaToken: "e2e-runtime-token", GiteaServerUrl: s.baseURL,
		Secrets:     []*codespacev1.RuntimeSecretEnvironmentVariable{{Name: "E2E_SECRET", Value: "e2e-secret-value"}},
		GitSshTrust: &codespacev1.GitSSHTrust{},
	}}), nil
}

func (s *lifecycleRemote) RequestIdleStop(_ context.Context, request *connect.Request[codespacev1.RequestIdleStopRequest]) (*connect.Response[codespacev1.RequestIdleStopResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	observed := request.Msg.ObservedSettings
	if request.Msg.ProtocolVersion != 1 || request.Msg.RuntimeUuid != s.runtimeUUID || observed == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid idle stop request"))
	}
	if observed.AutoStopEnabled != s.runtimeSettings.autoStopEnabled || observed.IdleTimeoutSeconds != s.runtimeSettings.idleTimeoutSeconds || observed.InteractionGeneration != s.runtimeSettings.interactionGeneration {
		settings := &codespacev1.EffectiveCodespaceRuntimeSettings{AutoStopEnabled: s.runtimeSettings.autoStopEnabled, IdleTimeoutSeconds: s.runtimeSettings.idleTimeoutSeconds, InteractionGeneration: s.runtimeSettings.interactionGeneration}
		return connect.NewResponse(&codespacev1.RequestIdleStopResponse{Outcome: &codespacev1.RequestIdleStopResponse_ObservationChanged{ObservationChanged: &codespacev1.IdleStopObservationChanged{RuntimeSettings: settings}}}), nil
	}
	if s.operation == nil {
		s.operation = &codespacev1.OperationPayload{CodespaceId: 1, RuntimeUuid: s.runtimeUUID, OperationRversion: s.lastFinalized + 1, LeaseValidForMilliseconds: 60000, Command: &codespacev1.OperationPayload_Stop{Stop: &codespacev1.StopOperationPayload{}}}
		s.delivered = false
	}
	return connect.NewResponse(&codespacev1.RequestIdleStopResponse{Outcome: &codespacev1.RequestIdleStopResponse_Pending{Pending: &codespacev1.IdleStopPending{OperationRversion: s.operation.OperationRversion}}}), nil
}

func (s *lifecycleRemote) ReportInstances(_ context.Context, request *connect.Request[codespacev1.ReportInstancesRequest]) (*connect.Response[codespacev1.ReportInstancesResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	response := &codespacev1.ReportInstancesResponse{}
	for _, instance := range request.Msg.Instances {
		if instance.RuntimeUuid != s.runtimeUUID {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("unknown runtime inventory"))
		}
		response.Results = append(response.Results, &codespacev1.RuntimeInstanceResult{
			RuntimeUuid:              instance.RuntimeUuid,
			RuntimeSettings:          &codespacev1.EffectiveCodespaceRuntimeSettings{AutoStopEnabled: s.runtimeSettings.autoStopEnabled, IdleTimeoutSeconds: s.runtimeSettings.idleTimeoutSeconds, InteractionGeneration: s.runtimeSettings.interactionGeneration},
			CurrentOperationRversion: max(s.lastFinalized, instance.ObservedOperationRversion),
		})
	}
	return connect.NewResponse(response), nil
}

func (s *lifecycleRemote) ValidateOpenToken(_ context.Context, request *connect.Request[codespacev1.ValidateOpenTokenRequest]) (*connect.Response[codespacev1.ValidateOpenTokenResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Msg.ProtocolVersion != 1 || request.Msg.Code != "lifecycle-e2e" || s.runtimeUUID == "" {
		return connect.NewResponse(&codespacev1.ValidateOpenTokenResponse{Outcome: &codespacev1.ValidateOpenTokenResponse_Denied{Denied: &codespacev1.FailureDetail{Category: "invalid_code"}}}), nil
	}
	return connect.NewResponse(&codespacev1.ValidateOpenTokenResponse{Outcome: &codespacev1.ValidateOpenTokenResponse_Allowed{Allowed: &codespacev1.OpenTokenBinding{
		UserId: 1, RuntimeUuid: s.runtimeUUID, EndpointId: "workspace", InteractionGeneration: s.interactionGeneration,
	}}}), nil
}

func (s *lifecycleRemote) RevalidateGatewaySession(_ context.Context, request *connect.Request[codespacev1.RevalidateGatewaySessionRequest]) (*connect.Response[codespacev1.RevalidateGatewaySessionResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	endpoint := request.Msg.GetEndpoint()
	sshSession := request.Msg.GetSsh()
	endpointAllowed := endpoint != nil && endpoint.UserId == 1 && endpoint.RuntimeUuid == s.runtimeUUID && endpoint.EndpointId == "workspace"
	sshAllowed := sshSession != nil && sshSession.UserId == 1 && sshSession.RuntimeUuid == s.runtimeUUID
	if request.Msg.ProtocolVersion != 1 || (!endpointAllowed && !sshAllowed) {
		return connect.NewResponse(&codespacev1.RevalidateGatewaySessionResponse{Outcome: &codespacev1.RevalidateGatewaySessionResponse_Denied{Denied: &codespacev1.FailureDetail{Category: "invalid_session"}}}), nil
	}
	return connect.NewResponse(&codespacev1.RevalidateGatewaySessionResponse{Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{Allowed: &codespacev1.SessionAllowed{}}}), nil
}

func (s *lifecycleRemote) ValidatePublicEndpoint(_ context.Context, request *connect.Request[codespacev1.ValidatePublicEndpointRequest]) (*connect.Response[codespacev1.ValidatePublicEndpointResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Msg.ProtocolVersion == 1 && request.Msg.RuntimeUuid == s.runtimeUUID && s.metadata != nil {
		for _, endpoint := range s.metadata.Endpoints {
			if endpoint.EndpointId == request.Msg.EndpointId && endpoint.Public {
				return connect.NewResponse(&codespacev1.ValidatePublicEndpointResponse{Outcome: &codespacev1.ValidatePublicEndpointResponse_Allowed{Allowed: &codespacev1.PublicEndpointAllowed{}}}), nil
			}
		}
	}
	return connect.NewResponse(&codespacev1.ValidatePublicEndpointResponse{Outcome: &codespacev1.ValidatePublicEndpointResponse_Denied{Denied: &codespacev1.FailureDetail{Category: "endpoint_unavailable"}}}), nil
}

func (s *lifecycleRemote) VerifySSHPublicKey(_ context.Context, request *connect.Request[codespacev1.VerifySSHPublicKeyRequest]) (*connect.Response[codespacev1.VerifySSHPublicKeyResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Msg.ProtocolVersion != 1 || request.Msg.RuntimeUuid != s.runtimeUUID || !bytes.Equal(request.Msg.PublicKey, s.sshPublicKey) {
		return connect.NewResponse(&codespacev1.VerifySSHPublicKeyResponse{Outcome: &codespacev1.VerifySSHPublicKeyResponse_Denied{Denied: &codespacev1.FailureDetail{Category: "invalid_key"}}}), nil
	}
	return connect.NewResponse(&codespacev1.VerifySSHPublicKeyResponse{Outcome: &codespacev1.VerifySSHPublicKeyResponse_Allowed{Allowed: &codespacev1.SSHAuthBinding{UserId: 1, InteractionGeneration: s.interactionGeneration}}}), nil
}

func (s *lifecycleRemote) waitFinal(t *testing.T, version int64, operationType codespacev1.OperationType, timeout time.Duration) {
	t.Helper()
	select {
	case result := <-s.finalized:
		require.Equal(t, version, result.OperationRversion)
		require.Equal(t, operationType, result.OperationType)
		if result.Status != codespacev1.FinalStatus_FINAL_STATUS_DONE {
			s.mu.Lock()
			logs := append([]string(nil), s.logs[version]...)
			s.mu.Unlock()
			if len(logs) > 80 {
				logs = logs[len(logs)-80:]
			}
			t.Fatalf("operation %d finalized as %s; recent lifecycle log:\n%s", version, result.Status, strings.Join(logs, "\n"))
		}
	case <-time.After(timeout):
		s.mu.Lock()
		logs := append([]string(nil), s.logs[version]...)
		s.mu.Unlock()
		if len(logs) > 80 {
			logs = logs[len(logs)-80:]
		}
		t.Fatalf("operation %d was not finalized within %s; recent lifecycle log:\n%s", version, timeout, strings.Join(logs, "\n"))
	}
}

func TestKubernetesE2EProductLifecycle(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_KUBERNETES_LIFECYCLE") != "1" {
		t.Skip("requires the deployed Manager, cert-manager, a supported RuntimeClass and outbound network access")
	}
	runtimeIsolation := os.Getenv("CODESPACE_TEST_RUNTIME_ISOLATION")
	runtimeClass := os.Getenv("CODESPACE_TEST_RUNTIME_CLASS")
	storageClass := os.Getenv("CODESPACE_TEST_STORAGE_CLASS")
	require.Contains(t, []string{"kata", "sysbox"}, runtimeIsolation)
	require.NotEmpty(t, runtimeClass)
	require.NotEmpty(t, storageClass)
	listenAddress := os.Getenv("CODESPACE_TEST_GITEA_LISTEN")
	nodeAddress := os.Getenv("CODESPACE_TEST_NODE_ADDRESS")
	require.NotEmpty(t, listenAddress, "CODESPACE_TEST_GITEA_LISTEN must be reachable from Runtime Pods")
	require.NotEmpty(t, net.ParseIP(nodeAddress), "CODESPACE_TEST_NODE_ADDRESS must be a Kubernetes node IP")

	listener, err := net.Listen("tcp4", listenAddress)
	require.NoError(t, err)
	host, rawPort, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.ParseInt(rawPort, 10, 32)
	require.NoError(t, err)
	remote := newLifecycleRemote()
	remote.baseURL = "http://" + listener.Addr().String()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sshSigner, err := ssh.NewSignerFromKey(privateKey)
	require.NoError(t, err)
	remote.sshPublicKey = sshSigner.PublicKey().Marshal()
	repositoryDirectory, commit := lifecycleGitRepository(t)
	path, handler := codespacev1connect.NewManagerServiceHandler(remote)
	mux := http.NewServeMux()
	mux.Handle("/api/codespace"+path, http.StripPrefix("/api/codespace", handler))
	mux.Handle("/repo.git/", http.StripPrefix("/repo.git/", http.FileServer(http.Dir(repositoryDirectory))))
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/codespace/") && (r.Header.Get("x-codespace-manager-id") != "1" || r.Header.Get("x-codespace-manager-secret") != strings.Repeat("e2e-site-secret-", 3)) {
			http.Error(w, "invalid Manager credential", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 10 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		require.ErrorIs(t, <-serveDone, http.ErrServerClosed)
	})

	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	c, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Minute)
	defer cancel()
	const managementNamespace = "codespace-system"
	admin := &AdminServer{Client: c, Namespace: managementNamespace}
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	templateName, gatewayName, cacheName, siteName := "e2e-env-"+suffix, "e2e-gateway-"+suffix, "e2e-cache-"+suffix, "e2e-site-"+suffix
	namespace := "codespace-" + siteName
	externalGatewayService := "e2e-gateway-nodeport-" + suffix
	t.Cleanup(func() {
		cleanupLifecycleResources(t, c, managementNamespace, siteName, namespace, templateName, gatewayName, cacheName, externalGatewayService)
	})

	runtimeConfiguration := testEnvironment()
	runtimeConfiguration.Isolation = runtimeIsolation
	runtimeConfiguration.RuntimeClassName = runtimeClass
	runtimeConfiguration.StorageClassName = storageClass
	if runtimeIsolation == "kata" {
		runtimeConfiguration.VolumeMode = corev1.PersistentVolumeBlock
		runtimeConfiguration.Storage[corev1.ResourceStorage] = resource.MustParse("6Gi")
	}
	templateSpec, err := json.Marshal(api.EnvironmentTemplateSpec{Tag: "standard", Description: "General development environment", Runtime: runtimeConfiguration})
	require.NoError(t, err)
	template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: templateName}}
	require.NoError(t, admin.saveTemplate(ctx, template, adminWrite{Name: templateName, Spec: templateSpec, Verification: "product lifecycle E2E"}, true))
	require.Eventually(t, func() bool {
		return c.Get(ctx, client.ObjectKeyFromObject(template), template) == nil && meta.IsStatusConditionTrue(template.Status.Conditions, "Ready")
	}, time.Minute, time.Second)

	gatewayConfig := configpkg.DefaultGatewayConfig()
	gatewayConfig.HTTP.PublicURL = "http://" + gatewayName + ".test"
	gatewayConfig.SSH.PublicAddr = gatewayName + ".test:22"
	gatewaySpec, err := json.Marshal(adminComponentSpec{Role: "gateway", DisplayName: "Lifecycle E2E gateway", Gateway: &gatewayConfig})
	require.NoError(t, err)
	gateway := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: gatewayName}}
	require.NoError(t, admin.saveComponent(ctx, gateway, adminWrite{Name: gatewayName, Spec: gatewaySpec}, true))

	cacheConfig := configpkg.CacheConfig{
		Enabled: true, Listen: ":5000", PublicURL: "http://" + cacheName + ".test", MaxSize: "4GiB", MaxAge: configpkg.Duration(24 * time.Hour), GCInterval: configpkg.Duration(time.Hour),
		Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: "/var/lib/codespace-cache", MinFreeSpace: "128MiB"},
		Upstreams: map[string]configpkg.RuntimeCacheUpstreamConfig{
			"docker.io": {Allow: []string{"library/debian*"}},
			"ghcr.io":   {Allow: []string{"coder/devcontainer-features/code-server*", "devcontainers/features/common-utils*"}},
		},
	}
	cacheSpec, err := json.Marshal(adminComponentSpec{Role: "cache", DisplayName: "Lifecycle E2E cache", Cache: &cacheConfig})
	require.NoError(t, err)
	cache := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cacheName}}
	require.NoError(t, admin.saveComponent(ctx, cache, adminWrite{Name: cacheName, Spec: cacheSpec}, true))

	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: gatewayName}, gateway))
	nodePortService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: externalGatewayService, Namespace: managementNamespace},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{ComponentLabel: "gateway", ComponentUIDLabel: string(gateway.UID)},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 18081, TargetPort: intstr.FromInt32(18081)},
				{Name: "ssh", Port: 2222, TargetPort: intstr.FromInt32(2222)},
			},
		},
	}
	require.NoError(t, c.Create(ctx, nodePortService))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(nodePortService), nodePortService))
	require.NotZero(t, nodePortService.Spec.Ports[0].NodePort)
	require.NotZero(t, nodePortService.Spec.Ports[1].NodePort)
	gatewayConfig.HTTP.PublicURL = fmt.Sprintf("http://%s.test:%d", gatewayName, nodePortService.Spec.Ports[0].NodePort)
	gatewayConfig.SSH.PublicAddr = net.JoinHostPort(gatewayName+".test", strconv.Itoa(int(nodePortService.Spec.Ports[1].NodePort)))
	gatewaySpec, err = json.Marshal(adminComponentSpec{Role: "gateway", DisplayName: "Lifecycle E2E gateway", Gateway: &gatewayConfig})
	require.NoError(t, err)
	require.NoError(t, admin.saveComponent(ctx, gateway, adminWrite{Name: gatewayName, Spec: gatewaySpec}, false))

	for _, name := range []string{gatewayName, cacheName} {
		require.Eventually(t, func() bool {
			var deployment appsv1.Deployment
			if c.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: name}, &deployment) != nil {
				return false
			}
			return deployment.Status.ObservedGeneration == deployment.Generation && deployment.Status.ReadyReplicas == 1
		}, 3*time.Minute, time.Second, "component %s did not become ready", name)
	}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: cacheName}, cache))

	quota := corev1.ResourceList{
		corev1.ResourcePods: resource.MustParse("2"), corev1.ResourcePersistentVolumeClaims: resource.MustParse("2"), corev1.ResourceRequestsStorage: resource.MustParse("10Gi"),
		corev1.ResourceRequestsCPU: resource.MustParse("2"), corev1.ResourceLimitsCPU: resource.MustParse("2"), corev1.ResourceRequestsMemory: resource.MustParse("2Gi"), corev1.ResourceLimitsMemory: resource.MustParse("2Gi"),
	}
	siteSpec := adminSiteSpec{GiteaSiteSpec: api.GiteaSiteSpec{
		DisplayName: "Lifecycle E2E site", URL: remote.baseURL, Enabled: true, AcceptCreates: true, StartupConcurrency: 1, CleanupConcurrency: 1,
		Templates: []api.ResourceReference{{Name: template.Name, UID: template.UID}}, Gateway: api.ResourceReference{Name: gateway.Name, UID: gateway.UID}, Caches: []api.ResourceReference{{Name: cache.Name, UID: cache.UID}},
		Quota: quota, ContainerLimits: corev1.LimitRangeItem{Type: corev1.LimitTypeContainer},
		Upstreams: []api.NetworkDestination{{CIDR: host + "/32", Ports: []int32{int32(port)}}, {CIDR: "0.0.0.0/0", Ports: []int32{80, 443}}},
	}, ManagerID: "1"}
	encodedSite, err := json.Marshal(siteSpec)
	require.NoError(t, err)
	site := &api.GiteaSite{ObjectMeta: metav1.ObjectMeta{Name: siteName}}
	require.NoError(t, admin.saveSite(ctx, site, adminWrite{Name: siteName, Spec: encodedSite, ManagerSecret: strings.Repeat("e2e-site-secret-", 3)}, true))
	require.Eventually(t, func() bool {
		return c.Get(ctx, client.ObjectKeyFromObject(site), site) == nil && meta.IsStatusConditionTrue(site.Status.Conditions, "InfrastructureReady") && site.Status.CanonicalURL == remote.baseURL
	}, 2*time.Minute, time.Second)
	select {
	case <-remote.managerOnline:
	case <-time.After(2 * time.Minute):
		t.Fatal("site coordinator did not become online")
	}

	remote.setOperation(&codespacev1.OperationPayload{
		CodespaceId: 1, OperationRversion: 1, LeaseValidForMilliseconds: 60000,
		Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{
			EnvironmentTag:  "standard",
			Repository:      &codespacev1.RepositoryCheckout{FullName: "tester/repository", CloneHttpUrl: remote.baseURL + "/repo.git", PreferredProtocol: codespacev1.GitProtocol_GIT_PROTOCOL_HTTP, StartRef: "refs/heads/main", CommitSha: commit, RepositoryId: 1},
			GitIdentity:     &codespacev1.GitIdentity{GiteaUsername: "tester", GitUserEmail: "tester@example.test", UserId: 1},
			DevContainer:    &codespacev1.DevContainerConfiguration{Source: &codespacev1.DevContainerConfiguration_TemplateContent{TemplateContent: `{"image":"docker.io/library/debian:bookworm-slim","remoteUser":"root"}`}},
			RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{InteractionGeneration: 1},
		}},
	})
	remote.waitFinal(t, 1, codespacev1.OperationType_OPERATION_TYPE_CREATE, 90*time.Minute)
	remote.mu.Lock()
	createLog := strings.Join(remote.logs[1], "\n")
	remote.mu.Unlock()
	require.Contains(t, createLog, "Dev Container Feature ghcr.io/devcontainers/features/common-utils:2 fetched through mirror")
	require.Contains(t, createLog, "Dev Container Feature ghcr.io/coder/devcontainer-features/code-server:2.0.0 fetched through mirror")
	require.Contains(t, createLog, "Dev Container features image cache published")
	if !strings.Contains(createLog, "Dev Container features build cache published") {
		t.Fatalf("BuildKit registry cache was not published; create log:\n%s", createLog)
	}

	key := types.NamespacedName{Namespace: namespace, Name: "codespace-1"}
	var codespace api.Codespace
	require.Eventually(t, func() bool {
		return c.Get(ctx, key, &codespace) == nil && codespace.Status.SettledOperationVersion == 1 && codespace.Status.Target != nil && codespace.Status.Target.Ready
	}, time.Minute, time.Second)
	firstPodUID, volumeUID, runtimeUUID := codespace.Status.Pod.UID, codespace.Status.Volume.UID, codespace.Spec.RuntimeUUID
	require.NotEmpty(t, firstPodUID)
	require.NotEmpty(t, volumeUID)
	require.NoError(t, verifyGatewayWorkspace(ctx, nodeAddress, nodePortService.Spec.Ports[0].NodePort, gatewayName+".test", runtimeUUID))
	require.NoError(t, verifyGatewaySSH(ctx, nodeAddress, nodePortService.Spec.Ports[1].NodePort, runtimeUUID, sshSigner, true))
	require.NoError(t, verifyGatewayPublicEndpoint(ctx, nodeAddress, nodePortService.Spec.Ports[0].NodePort, gatewayName+".test", runtimeUUID, true))

	remote.setOperation(&codespacev1.OperationPayload{CodespaceId: 1, RuntimeUuid: runtimeUUID, OperationRversion: 2, LeaseValidForMilliseconds: 60000, Command: &codespacev1.OperationPayload_Stop{Stop: &codespacev1.StopOperationPayload{}}})
	remote.waitFinal(t, 2, codespacev1.OperationType_OPERATION_TYPE_STOP, 3*time.Minute)
	require.Eventually(t, func() bool {
		return c.Get(ctx, key, &codespace) == nil && codespace.Status.SettledOperationVersion == 2 && meta.IsStatusConditionTrue(codespace.Status.Conditions, "Stopped") && codespace.Status.Pod.UID == ""
	}, time.Minute, time.Second)
	require.Equal(t, volumeUID, codespace.Status.Volume.UID)
	require.NoError(t, verifyGatewayWorkspaceUnavailable(ctx, nodeAddress, nodePortService.Spec.Ports[0].NodePort, gatewayName+".test", runtimeUUID))

	remote.setOperation(&codespacev1.OperationPayload{CodespaceId: 1, RuntimeUuid: runtimeUUID, OperationRversion: 3, LeaseValidForMilliseconds: 60000, Command: &codespacev1.OperationPayload_Resume{Resume: &codespacev1.ResumeOperationPayload{RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{InteractionGeneration: 2}}}})
	remote.waitFinal(t, 3, codespacev1.OperationType_OPERATION_TYPE_RESUME, 8*time.Minute)
	require.Eventually(t, func() bool {
		return c.Get(ctx, key, &codespace) == nil && codespace.Status.SettledOperationVersion == 3 && codespace.Status.Target != nil && codespace.Status.Target.Ready
	}, time.Minute, time.Second)
	require.NotEqual(t, firstPodUID, codespace.Status.Pod.UID)
	require.Equal(t, volumeUID, codespace.Status.Volume.UID)
	require.NoError(t, verifyGatewayWorkspace(ctx, nodeAddress, nodePortService.Spec.Ports[0].NodePort, gatewayName+".test", runtimeUUID))
	require.NoError(t, verifyGatewayPublicEndpoint(ctx, nodeAddress, nodePortService.Spec.Ports[0].NodePort, gatewayName+".test", runtimeUUID, true))
	require.NoError(t, verifyGatewaySSH(ctx, nodeAddress, nodePortService.Spec.Ports[1].NodePort, runtimeUUID, sshSigner, false))
	require.NoError(t, verifyGatewayPublicEndpoint(ctx, nodeAddress, nodePortService.Spec.Ports[0].NodePort, gatewayName+".test", runtimeUUID, false))

	remote.setRuntimeSettings(&codespacev1.EffectiveCodespaceRuntimeSettings{AutoStopEnabled: true, IdleTimeoutSeconds: 2, InteractionGeneration: 3})
	remote.waitFinal(t, 4, codespacev1.OperationType_OPERATION_TYPE_STOP, time.Minute)
	require.Eventually(t, func() bool {
		return c.Get(ctx, key, &codespace) == nil && codespace.Status.SettledOperationVersion == 4 && meta.IsStatusConditionTrue(codespace.Status.Conditions, "Stopped") && codespace.Status.Pod.UID == ""
	}, time.Minute, time.Second)

	remote.setOperation(&codespacev1.OperationPayload{CodespaceId: 1, RuntimeUuid: runtimeUUID, OperationRversion: 5, LeaseValidForMilliseconds: 60000, Command: &codespacev1.OperationPayload_Delete{Delete: &codespacev1.DeleteOperationPayload{}}})
	remote.waitFinal(t, 5, codespacev1.OperationType_OPERATION_TYPE_DELETE, 3*time.Minute)
	require.Eventually(t, func() bool { return apierrors.IsNotFound(c.Get(ctx, key, &api.Codespace{})) }, 2*time.Minute, time.Second)
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "data-" + runtimeUUID}, &corev1.PersistentVolumeClaim{}))
	}, 2*time.Minute, time.Second)
}

func lifecycleGitRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	worktree, bare := filepath.Join(root, "worktree"), filepath.Join(root, "repo.git")
	require.NoError(t, os.Mkdir(worktree, 0o755))
	run := func(directory string, arguments ...string) string {
		command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return strings.TrimSpace(string(output))
	}
	run(worktree, "init", "-b", "main")
	run(worktree, "config", "user.name", "Lifecycle E2E")
	run(worktree, "config", "user.email", "e2e@example.test")
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "README.md"), []byte("# Lifecycle E2E\n"), 0o644))
	run(worktree, "add", "README.md")
	run(worktree, "commit", "-m", "initial")
	commit := run(worktree, "rev-parse", "HEAD")
	command := exec.Command("git", "clone", "--bare", worktree, bare)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	run(bare, "update-server-info")
	return bare, commit
}

func verifyGatewayWorkspace(ctx context.Context, node string, port int32, domain, runtimeUUID string) error {
	host := strings.ReplaceAll(runtimeUUID, "-", "") + "." + domain + ":" + strconv.Itoa(int(port))
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	var lastError error
	for range 90 {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/.gitea-codespace/open?code=lifecycle-e2e", node, port), nil)
		if err != nil {
			return err
		}
		request.Host = host
		response, err := client.Do(request)
		if err == nil && response.StatusCode == http.StatusSeeOther && len(response.Cookies()) != 0 {
			_ = response.Body.Close()
			request, err = http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/", node, port), nil)
			if err != nil {
				return err
			}
			request.Host = host
			request.AddCookie(response.Cookies()[0])
			response, err = client.Do(request)
			if err == nil {
				body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
				_ = response.Body.Close()
				if readErr == nil && response.StatusCode >= 200 && response.StatusCode < 500 && response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusNotFound {
					return nil
				}
				lastError = fmt.Errorf("gateway returned %s: %s", response.Status, strings.TrimSpace(string(body)))
			}
		} else {
			if response != nil {
				body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
				_ = response.Body.Close()
				lastError = fmt.Errorf("gateway open returned %s: %s", response.Status, strings.TrimSpace(string(body)))
			} else {
				lastError = err
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(lastError, ctx.Err())
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("gateway workspace did not become available: %w", lastError)
}

func verifyGatewayWorkspaceUnavailable(ctx context.Context, node string, port int32, domain, runtimeUUID string) error {
	host := strings.ReplaceAll(runtimeUUID, "-", "") + "." + domain + ":" + strconv.Itoa(int(port))
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/.gitea-codespace/open?code=lifecycle-e2e", node, port), nil)
	if err != nil {
		return err
	}
	request.Host = host
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || len(response.Cookies()) == 0 {
		return fmt.Errorf("gateway did not create the route-removal test session: %s", response.Status)
	}
	session := response.Cookies()[0]
	var lastStatus string
	for range 30 {
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/", node, port), nil)
		if err != nil {
			return err
		}
		request.Host = host
		request.AddCookie(session)
		response, err = client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			lastStatus = response.Status
			if response.StatusCode == http.StatusServiceUnavailable {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("gateway retained a workspace route after the Runtime stopped; last response was %s", lastStatus)
}

func verifyGatewaySSH(ctx context.Context, node string, port int32, runtimeUUID string, signer ssh.Signer, initialize bool) error {
	address := net.JoinHostPort(node, strconv.Itoa(int(port)))
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect Gateway SSH: %w", err)
	}
	defer func() { _ = connection.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, address, &ssh.ClientConfig{
		User:            "cs-" + runtimeUUID,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // The E2E uses an ephemeral Gateway host key.
	})
	if err != nil {
		return fmt.Errorf("authenticate Gateway SSH: %w", err)
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	defer func() { _ = client.Close() }()

	run := func(command string) ([]byte, error) {
		if err := connection.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			return nil, err
		}
		session, err := client.NewSession()
		if err != nil {
			return nil, err
		}
		defer func() { _ = session.Close() }()
		return session.CombinedOutput(command)
	}
	if initialize {
		output, err := run(`test "$E2E_SECRET" = "e2e-secret-value" && test "$(git branch --show-current)" = "main" && printf lifecycle-persisted > .codespace-lifecycle && printf command-ok`)
		if err != nil {
			return fmt.Errorf("run Gateway SSH command: output %q: %w", output, err)
		}
		if string(output) != "command-ok" {
			return fmt.Errorf("run Gateway SSH command: unexpected output %q", output)
		}
		if err := connection.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			return fmt.Errorf("set Gateway SSH PTY deadline: %w", err)
		}
		session, err := client.NewSession()
		if err != nil {
			return err
		}
		if err := session.RequestPty("xterm-256color", 37, 91, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
			_ = session.Close()
			return fmt.Errorf("request Gateway SSH PTY: %w", err)
		}
		stdin, err := session.StdinPipe()
		if err != nil {
			_ = session.Close()
			return err
		}
		stdout, err := session.StdoutPipe()
		if err != nil {
			_ = session.Close()
			return err
		}
		if err := session.Start(`stty -echo; stty size; while read value; do test "$value" = done && exit; stty size; done`); err != nil {
			_ = session.Close()
			return fmt.Errorf("start Gateway SSH PTY resize check: %w", err)
		}
		reader := bufio.NewReader(stdout)
		initialSize, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(initialSize) != "37 91" {
			_ = session.Close()
			return fmt.Errorf("verify Gateway SSH initial PTY size: output %q: %w", initialSize, err)
		}
		if err := session.WindowChange(43, 107); err != nil {
			_ = session.Close()
			return fmt.Errorf("resize Gateway SSH PTY: %w", err)
		}
		var resized string
		for range 20 {
			if _, err := io.WriteString(stdin, "probe\n"); err != nil {
				_ = session.Close()
				return err
			}
			resized, err = reader.ReadString('\n')
			if err != nil || strings.TrimSpace(resized) == "43 107" {
				break
			}
		}
		if err != nil || strings.TrimSpace(resized) != "43 107" {
			_ = session.Close()
			return fmt.Errorf("verify Gateway SSH resized PTY: output %q: %v", resized, err)
		}
		if _, err := io.WriteString(stdin, "done\n"); err != nil {
			_ = session.Close()
			return err
		}
		if err := session.Wait(); err != nil {
			_ = session.Close()
			return fmt.Errorf("finish Gateway SSH PTY resize check: %w", err)
		}
		_ = session.Close()
		if err := connection.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			return fmt.Errorf("set Gateway SFTP deadline: %w", err)
		}
		files, err := sftp.NewClient(client)
		if err != nil {
			return fmt.Errorf("start Gateway SFTP: %w", err)
		}
		file, err := files.Create(".codespace-sftp")
		if err == nil {
			_, err = file.Write([]byte("sftp-persisted"))
			err = errors.Join(err, file.Close())
		}
		err = errors.Join(err, files.Close())
		if err != nil {
			return fmt.Errorf("write through Gateway SFTP: %w", err)
		}
		if err := connection.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			return fmt.Errorf("set Gateway forwarding deadline: %w", err)
		}
		for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
			endpoint, err := client.Dial("tcp", net.JoinHostPort(host, "13337"))
			if err != nil {
				return fmt.Errorf("open Gateway loopback forwarding for %s: %w", host, err)
			}
			_, writeErr := io.WriteString(endpoint, "GET / HTTP/1.0\r\nHost: localhost\r\n\r\n")
			response, readErr := http.ReadResponse(bufio.NewReader(endpoint), nil)
			if readErr == nil {
				_, readErr = io.ReadAll(io.LimitReader(response.Body, 4096))
				readErr = errors.Join(readErr, response.Body.Close())
			}
			closeErr := endpoint.Close()
			if errors.Is(closeErr, io.EOF) {
				closeErr = nil
			}
			if err := errors.Join(writeErr, readErr, closeErr); err != nil || response.ProtoMajor != 1 {
				return fmt.Errorf("verify Gateway loopback forwarding for %s: %w", host, err)
			}
		}
		output, err = run("gitea-codespace-endpoint set 13337 --label Preview --public")
		if err != nil {
			return fmt.Errorf("publish runtime endpoint: output %q: %w", output, err)
		}
		return nil
	}
	output, err := run(`test "$(cat .codespace-lifecycle)" = "lifecycle-persisted" && test "$(cat .codespace-sftp)" = "sftp-persisted" && printf resumed-ok`)
	if err != nil {
		return fmt.Errorf("verify resumed Gateway SSH workspace: output %q: %w", output, err)
	}
	if string(output) != "resumed-ok" {
		return fmt.Errorf("verify resumed Gateway SSH workspace: unexpected output %q", output)
	}
	output, err = run("gitea-codespace-endpoint delete 13337")
	if err != nil {
		return fmt.Errorf("remove runtime endpoint: output %q: %w", output, err)
	}
	return nil
}

func verifyGatewayPublicEndpoint(ctx context.Context, node string, port int32, domain, runtimeUUID string, available bool) error {
	host := "port-13337-" + strings.ReplaceAll(runtimeUUID, "-", "") + "." + domain + ":" + strconv.Itoa(int(port))
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	var lastStatus string
	for range 30 {
		path := "/"
		if !available {
			path = "/p/" + runtimeUUID + "/port-13337/"
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d%s", node, port, path), nil)
		if err != nil {
			return err
		}
		request.Host = host
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			lastStatus = response.Status
			if (available && response.StatusCode >= 200 && response.StatusCode < 400) || (!available && response.StatusCode == http.StatusNotFound) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("gateway public endpoint availability %t was not observed; last response was %s", available, lastStatus)
}

func cleanupLifecycleResources(t *testing.T, c client.Client, managementNamespace, siteName, namespace, templateName, gatewayName, cacheName, externalService string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var ns corev1.Namespace
	if c.Get(ctx, types.NamespacedName{Name: namespace}, &ns) == nil {
		_ = c.Delete(ctx, &ns, client.Preconditions{UID: &ns.UID})
		for range 60 {
			if apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: namespace}, &ns)) {
				break
			}
			time.Sleep(time.Second)
		}
		if c.Get(ctx, types.NamespacedName{Name: namespace}, &ns) == nil {
			for range 60 {
				var pods corev1.PodList
				if c.List(ctx, &pods, client.InNamespace(namespace)) == nil {
					for i := range pods.Items {
						pods.Items[i].Finalizers = nil
						_ = c.Update(ctx, &pods.Items[i])
					}
				}
				var codespaces api.CodespaceList
				if c.List(ctx, &codespaces, client.InNamespace(namespace)) == nil {
					for i := range codespaces.Items {
						codespaces.Items[i].Finalizers = nil
						_ = c.Update(ctx, &codespaces.Items[i])
					}
				}
				if apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: namespace}, &ns)) {
					break
				}
				time.Sleep(time.Second)
			}
		}
	}
	var site api.GiteaSite
	if c.Get(ctx, types.NamespacedName{Name: siteName}, &site) == nil {
		site.Spec.Enabled = false
		_ = c.Update(ctx, &site)
		site.Finalizers = nil
		_ = c.Update(ctx, &site)
		_ = c.Delete(ctx, &site, client.Preconditions{UID: &site.UID})
	}
	var service corev1.Service
	if c.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: externalService}, &service) == nil {
		_ = c.Delete(ctx, &service, client.Preconditions{UID: &service.UID})
	}
	for _, name := range []string{gatewayName, cacheName} {
		var component corev1.ConfigMap
		if c.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: name}, &component) == nil {
			_ = c.Delete(ctx, &component, client.Preconditions{UID: &component.UID})
		}
	}
	var template api.EnvironmentTemplate
	if c.Get(ctx, types.NamespacedName{Name: templateName}, &template) == nil {
		_ = c.Delete(ctx, &template, client.Preconditions{UID: &template.UID})
	}
	if err := c.Get(ctx, types.NamespacedName{Name: siteName}, &api.GiteaSite{}); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("verify lifecycle cleanup: %v", err)
	}
}

var _ codespacev1connect.ManagerServiceHandler = (*lifecycleRemote)(nil)
