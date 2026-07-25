// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/crypto/ssh"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
)

func TestAppE2EManagerProcessDeleteCleanupWithDummyProvisioner(t *testing.T) {
	codespaceUUID := "22222222-2222-4222-8222-222222222222"
	service := &appE2EManagerService{
		finalized: make(chan struct{}, 1),
		operation: &codespacev1.OperationPayload{
			OperationRversion:         1,
			CodespaceUuid:             codespaceUUID,
			LogOffset:                 0,
			LeaseValidForMilliseconds: 30000,
			Command: &codespacev1.OperationPayload_Delete{
				Delete: &codespacev1.DeleteOperationPayload{},
			},
		},
	}
	controlPlane := newGiteaManagerServiceServer(t, service)
	defer controlPlane.Close()

	stateDir := t.TempDir()
	saveManagerRegistrationForTest(t, stateDir, controlPlane.URL, 7)
	config := DefaultConfig()
	config.Manager.StateDir = stateDir
	config.Manager.PollInterval = Duration(10 * time.Millisecond)
	config.Manager.DeclareInterval = Duration(50 * time.Millisecond)
	config.Manager.HTTPTimeout = Duration(time.Second)
	config.Server.GatewayListenAddr = "127.0.0.1:0"
	config.Server.GatewaySSHListenAddr = "127.0.0.1:0"
	config.Manager.GatewayURL = "http://127.0.0.1"
	config.Manager.GatewaySSHAddr = "127.0.0.1:22"
	config.Server.ShutdownTimeout = Duration(time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	var output bytes.Buffer
	go func() {
		runDone <- runWithConfigContext(ctx, &output, config)
	}()
	waitAppE2EFinalized(t, service.finalized)
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run manager process: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("manager process did not stop")
	}
	if !service.sawDeclare() || !service.sawFetch() {
		t.Fatalf("service calls declare=%v fetch=%v output=%s", service.sawDeclare(), service.sawFetch(), output.String())
	}
	if service.finalStatus() != codespacev1.FinalStatus_FINAL_STATUS_DONE {
		t.Fatalf("final status = %s", service.finalStatus())
	}
}

func TestAppE2EManagerProcessIncusCreateStopResumeLifecycle(t *testing.T) {
	if !appE2EEnvBool("CODESPACE_E2E_INCUS_MANAGER_LIFECYCLE") {
		t.Skip("Manager process Incus lifecycle E2E is disabled; run make test-e2e-manager-required to enable it")
	}

	runSuffix := uint64(time.Now().UnixNano()) & 0xffffffffffff
	codespaceUUID := fmt.Sprintf("33333333-3333-4333-8333-%012x", runSuffix)
	managerID := time.Now().UnixNano()
	scripts := writeAppE2EIncusScripts(t)
	service := &appE2EManagerService{
		finalized: make(chan struct{}, 3),
		operations: []*codespacev1.OperationPayload{
			appE2ECreateOperation(codespaceUUID, 1),
			{
				OperationRversion:         2,
				CodespaceUuid:             codespaceUUID,
				LogOffset:                 0,
				LeaseValidForMilliseconds: 300000,
				Command: &codespacev1.OperationPayload_Stop{
					Stop: &codespacev1.StopOperationPayload{},
				},
			},
			{
				OperationRversion:         3,
				CodespaceUuid:             codespaceUUID,
				LogOffset:                 0,
				LeaseValidForMilliseconds: 300000,
				Command: &codespacev1.OperationPayload_Resume{
					Resume: &codespacev1.ResumeOperationPayload{
						RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{},
					},
				},
			},
		},
	}
	controlPlane := newGiteaManagerServiceServer(t, service)
	defer controlPlane.Close()

	stateDir := t.TempDir()
	saveManagerRegistrationForTest(t, stateDir, controlPlane.URL, managerID)
	defer cleanupAppE2EIncusRuntime(t, managerID, codespaceUUID)

	config := appE2EIncusManagerConfig(controlPlane.URL, stateDir, scripts)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	var output bytes.Buffer
	go func() {
		runDone <- runWithConfigContext(ctx, &output, config)
	}()

	waitAppE2EFinalizedCount(t, service.finalized, 3, 7*time.Minute)
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run manager process: %v\noutput:\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("manager process did not stop\noutput:\n%s", output.String())
	}
	if !service.sawDeclare() || !service.sawFetch() || !service.sawMetadata() {
		t.Fatalf("service calls declare=%v fetch=%v metadata=%v output=%s", service.sawDeclare(), service.sawFetch(), service.sawMetadata(), output.String())
	}
	for index, status := range service.finalStatuses() {
		if status != codespacev1.FinalStatus_FINAL_STATUS_DONE {
			t.Fatalf("final status[%d] = %s, want done\noperation log:\n%s\noutput:\n%s", index, status, service.operationLog(), output.String())
		}
	}
}

func TestAppE2ERuntimeEndpointGatewayHTTPAndSSH(t *testing.T) {
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	store := NewCodespaceStateStore(t.TempDir())
	routes := newGatewayRouteStore()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/health" {
			t.Fatalf("upstream path = %q", request.URL.Path)
		}
		if got := request.Header.Get(gatewayProxyHeaderCodespaceUUID); got != codespaceUUID {
			t.Fatalf("codespace header = %q", got)
		}
		if got := request.Header.Get(gatewayProxyHeaderEndpointID); got != "web" {
			t.Fatalf("endpoint header = %q", got)
		}
		if got := request.Header.Get(gatewayProxyHeaderAccess); got != "public" {
			t.Fatalf("access header = %q", got)
		}
		fmt.Fprint(writer, "endpoint ok")
	}))
	defer upstream.Close()
	upstreamHost, upstreamPort := splitTestHostPort(t, upstream.URL)

	service := &gatewayManagerService{
		publicEndpointResponse: allowedPublicEndpointResponse(),
		sshResponse: &codespacev1.VerifySSHPublicKeyResponse{
			Outcome: &codespacev1.VerifySSHPublicKeyResponse_Allowed{
				Allowed: &codespacev1.SSHAuthBinding{UserId: 42},
			},
		},
		revalidateResponse: allowedRevalidateResponse(),
	}
	controlPlane, closeControlPlane := newTestGatewayControlPlane(t, service)
	defer closeControlPlane()

	route := manager.RuntimeEndpointRoute{
		CodespaceUUID:  codespaceUUID,
		EndpointID:     "web",
		Label:          "Web",
		UpstreamScheme: "http",
		UpstreamHost:   net.JoinHostPort(upstreamHost, strconv.Itoa(upstreamPort)),
		Public:         true,
	}
	if _, err := store.SaveRuntimeEndpointRoutes(codespaceUUID, []manager.RuntimeEndpointRoute{route}); err != nil {
		t.Fatalf("save runtime endpoint routes: %v", err)
	}
	if err := routes.ReplaceRuntimeEndpointRoutes(codespaceUUID, []manager.RuntimeEndpointRoute{route}); err != nil {
		t.Fatalf("replace runtime endpoint routes: %v", err)
	}

	gateway := newGatewayHandlerWithOriginAndBrowserAuth(
		newProcessHealth(),
		newGatewaySessionRegistry(),
		newTestGatewayAccess(),
		controlPlane,
		gatewayOriginPolicy{},
		nil,
		routes,
	)
	publicEndpointResponse := httptest.NewRecorder()
	gateway.ServeHTTP(publicEndpointResponse, httptest.NewRequest(http.MethodGet, "/p/"+codespaceUUID+"/web/health", nil))
	if publicEndpointResponse.Code != http.StatusOK {
		t.Fatalf("gateway public endpoint status = %d body=%s", publicEndpointResponse.Code, publicEndpointResponse.Body.String())
	}
	if publicEndpointResponse.Body.String() != "endpoint ok" {
		t.Fatalf("gateway public endpoint body = %q", publicEndpointResponse.Body.String())
	}

	gatewayHostKey := newTestSSHSigner(t)
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 1,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       100,
			LastUpdateUnix:    100,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}
	gatewaySSH, err := newGatewaySSHServer(
		gatewayHostKey,
		store,
		newGatewayRouteStore(),
		newTestWorkspaceCommandBackend("internal ready\n"),
		controlPlane,
		newGatewaySessionRegistry(),
		newGatewayAccessControllerFromConfig(DefaultConfig().Gateway),
		DefaultConfig().Gateway,
	)
	if err != nil {
		t.Fatalf("create gateway ssh server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener, gatewaySSH)
	defer listener.Close()

	clientKey := newTestSSHSigner(t)
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User:            "cs-11111111111141118111111111111111",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("dial gateway ssh: %v", err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("open gateway ssh session: %v", err)
	}
	defer session.Close()
	output, err := session.Output("echo ready")
	if err != nil {
		t.Fatalf("run gateway ssh command: %v", err)
	}
	if string(output) != "internal ready\n" {
		t.Fatalf("gateway ssh output = %q", output)
	}
	select {
	case err := <-errorChannel:
		t.Fatalf("gateway ssh server stopped early: %v", err)
	default:
	}
}

type appE2EIncusScripts struct {
	init  string
	start string
	stop  string
}

func appE2ECreateOperation(codespaceUUID string, version int64) *codespacev1.OperationPayload {
	return &codespacev1.OperationPayload{
		OperationRversion:         version,
		CodespaceUuid:             codespaceUUID,
		LogOffset:                 0,
		LeaseValidForMilliseconds: 300000,
		Command: &codespacev1.OperationPayload_Create{
			Create: &codespacev1.CreateOperationPayload{
				RepoId:           1,
				RepoFullName:     "owner/repo",
				RepoName:         "repo",
				RepoCloneHttpUrl: "https://gitea.example.com/owner/repo.git",
				RepoWebUrl:       "https://gitea.example.com/owner/repo",
				OwnerId:          2,
				OwnerName:        "owner",
				OwnerType:        codespacev1.RepositoryOwnerType_REPOSITORY_OWNER_TYPE_USER,
				EnvironmentTag:   "default",
				RuntimeSettings:  &codespacev1.EffectiveCodespaceRuntimeSettings{},
				GitProtocol:      codespacev1.GitProtocol_GIT_PROTOCOL_HTTP,
				CommitSha:        "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
}

func appE2EIncusManagerConfig(controlPlaneURL, stateDir string, scripts appE2EIncusScripts) Config {
	config := DefaultConfig()
	config.Manager.StateDir = stateDir
	config.Manager.Name = "app-e2e-incus-manager"
	config.Manager.PollInterval = Duration(100 * time.Millisecond)
	config.Manager.DeclareInterval = Duration(200 * time.Millisecond)
	config.Manager.HTTPTimeout = Duration(10 * time.Second)
	config.Manager.CapacityTotal = 1
	config.Manager.StartupWorkers = 1
	config.Manager.CleanupWorkers = 1
	config.Manager.GatewayURL = "http://127.0.0.1"
	config.Manager.GatewaySSHAddr = "127.0.0.1:22"
	config.Server.GatewayListenAddr = "127.0.0.1:0"
	config.Server.GatewaySSHListenAddr = "127.0.0.1:0"
	config.Server.ShutdownTimeout = Duration(5 * time.Second)
	config.Scripts.Init = scripts.init
	config.Scripts.Start = scripts.start
	config.Scripts.Stop = scripts.stop
	config.Provisioner.Kind = "incus"
	config.Incus.Endpoint = strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_REMOTE"))
	config.Incus.UnixSocket = strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_UNIX_SOCKET"))
	config.Incus.Project = strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_PROJECT"))
	config.RuntimeEnvironments = map[string]RuntimeEnvironmentConfig{
		"default": {
			Image:                  appE2EEnvDefault("CODESPACE_E2E_INCUS_IMAGE", "images:debian/12"),
			InstanceType:           appE2EEnvDefault("CODESPACE_E2E_INCUS_INSTANCE_TYPE", "container"),
			CommunicationInterface: appE2EEnvDefault("CODESPACE_E2E_INCUS_COMMUNICATION_INTERFACE", "eth0"),
			CPU:                    1,
			MemoryLimit:            appE2EEnvDefault("CODESPACE_E2E_INCUS_MEMORY_LIMIT", "1GiB"),
			RootDiskSize:           appE2EEnvDefault("CODESPACE_E2E_INCUS_ROOT_DISK_SIZE", "10GiB"),
			Profiles:               appE2EIncusProfiles(),
		},
	}
	return config
}

func cleanupAppE2EIncusRuntime(t *testing.T, managerID int64, codespaceUUID string) {
	t.Helper()
	config := provisioner.IncusConfig{
		ManagerID:  managerID,
		Remote:     strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_REMOTE")),
		UnixSocket: strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_UNIX_SOCKET")),
		Project:    strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_PROJECT")),
		RuntimeEnvironments: map[string]provisioner.IncusEnvironmentConfig{
			"default": {
				Image:                  appE2EEnvDefault("CODESPACE_E2E_INCUS_IMAGE", "images:debian/12"),
				InstanceType:           appE2EEnvDefault("CODESPACE_E2E_INCUS_INSTANCE_TYPE", "container"),
				CPU:                    1,
				MemoryLimit:            appE2EEnvDefault("CODESPACE_E2E_INCUS_MEMORY_LIMIT", "1GiB"),
				RootDiskSize:           appE2EEnvDefault("CODESPACE_E2E_INCUS_ROOT_DISK_SIZE", "10GiB"),
				Profiles:               appE2EIncusProfiles(),
				CommunicationInterface: appE2EEnvDefault("CODESPACE_E2E_INCUS_COMMUNICATION_INTERFACE", "eth0"),
			},
		},
	}
	incusProvisioner, err := provisioner.NewIncus(config)
	if err != nil {
		t.Logf("cleanup incus provisioner unavailable: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	instances, err := incusProvisioner.ListInstances(ctx)
	if err != nil {
		t.Logf("list incus runtimes for cleanup: %v", err)
		return
	}
	for _, instance := range instances {
		if instance == nil || instance.CodespaceUUID != codespaceUUID {
			continue
		}
		if err := incusProvisioner.Delete(ctx, instance.Name); err != nil {
			t.Logf("cleanup incus runtime %s: %v", instance.Name, err)
		}
	}
}

func writeAppE2EIncusScripts(t *testing.T) appE2EIncusScripts {
	t.Helper()
	dir := t.TempDir()
	scripts := map[string]string{
		"init.sh":  appE2EIncusInitScript,
		"start.sh": appE2EIncusStartScript,
		"stop.sh":  appE2EIncusStopScript,
	}
	result := appE2EIncusScripts{}
	for name, content := range scripts {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		switch name {
		case "init.sh":
			result.init = path
		case "start.sh":
			result.start = path
		case "stop.sh":
			result.stop = path
		}
	}
	return result
}

func appE2EEnvBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func appE2EEnvDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func appE2ESplitList(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func appE2EIncusProfiles() []string {
	profiles := appE2ESplitList(os.Getenv("CODESPACE_E2E_INCUS_PROFILES"))
	if len(profiles) == 0 {
		return []string{"default"}
	}
	return profiles
}

const appE2EIncusInitScript = `
set -eu
write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"initialize-system"}\n' "$1" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}
fail_deployment() {
  printf '%s\n' "$1" >&2
  write_result unrecoverable_failed
  exit 0
}
command -v getent >/dev/null 2>&1 || fail_deployment "getent is required"
command -v groupadd >/dev/null 2>&1 || fail_deployment "groupadd is required"
command -v useradd >/dev/null 2>&1 || fail_deployment "useradd is required"
if getent group codespace >/dev/null 2>&1; then
  if [ "$(getent group codespace | cut -d: -f3)" != "1000" ]; then
    fail_deployment "codespace group exists with an unexpected gid"
  fi
else
  groupadd -g 1000 codespace
fi
if id -u codespace >/dev/null 2>&1; then
  if [ "$(id -u codespace)" != "1000" ] || [ "$(id -g codespace)" != "1000" ]; then
    fail_deployment "codespace user exists with an unexpected uid or gid"
  fi
else
  useradd -m -u 1000 -g 1000 -s /bin/sh codespace
fi
mkdir -p /workspaces /usr/local/bin
chown 1000:1000 /workspaces
chmod 0755 /workspaces
cat >/usr/local/bin/git <<'EOF'
#!/bin/sh
set -eu
if [ "${1:-}" = "-C" ]; then
  shift
  shift
fi
if [ "${1:-}" = "remote" ] && [ "${2:-}" = "get-url" ] && [ "${3:-}" = "origin" ]; then
  printf 'https://gitea.example.com/owner/repo.git\n'
  exit 0
fi
if [ "${1:-}" = "config" ]; then
  shift
  if [ "${1:-}" = "--global" ]; then
    shift
  fi
  if [ "${1:-}" = "--get" ] && [ "${2:-}" = "credential.helper" ]; then
    printf '!/usr/local/bin/gitea-codespace-git-credential\n'
    exit 0
  fi
  if [ "${1:-}" = "--get" ] && [ "${2:-}" = "core.sshCommand" ]; then
    exit 1
  fi
fi
exit 0
EOF
chmod 0755 /usr/local/bin/git
cat >/usr/local/bin/gitea-codespace-git-credential <<'EOF'
#!/bin/sh
set -eu
while IFS= read -r line; do
  [ -n "$line" ] || break
done
printf 'username=codespace\n'
printf 'password=%s\n\n' "$(cat /var/lib/gitea-codespace/gitea-token)"
EOF
chmod 0755 /usr/local/bin/gitea-codespace-git-credential
workspace="/workspaces/${CODESPACE_REPO_NAME:-repo}"
mkdir -p "$workspace/.git"
chown -R 1000:1000 "$workspace"
printf '%s\n' 'CODESPACE_CREDENTIAL_UID=1000' 'CODESPACE_CREDENTIAL_GID=1000' "CODESPACE_WORKSPACE_DIR=${workspace}" >> "$CODESPACE_ENV"
write_result done
`

const appE2EIncusStartScript = `
set -eu
write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"done","stage":"start-environment"}\n' > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}
[ -n "${CODESPACE_WORKSPACE_DIR:-}" ] || exit 65
[ -d "$CODESPACE_WORKSPACE_DIR/.git" ] || exit 66
printf 'CODESPACE_WORKSPACE_DIR=%s\n' "$CODESPACE_WORKSPACE_DIR" >> "$CODESPACE_ENV"
write_result
`

const appE2EIncusStopScript = `
set -eu
write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"done","stage":"stop-environment"}\n' > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}
write_result
`

type appE2EManagerService struct {
	codespacev1connect.UnimplementedManagerServiceHandler

	mu             sync.Mutex
	operation      *codespacev1.OperationPayload
	operations     []*codespacev1.OperationPayload
	declared       bool
	fetched        bool
	metadata       bool
	status         codespacev1.FinalStatus
	statuses       []codespacev1.FinalStatus
	logs           []string
	finalized      chan struct{}
	operationIndex int
}

func (s *appE2EManagerService) DeclareManager(
	_ context.Context,
	req *connect.Request[codespacev1.DeclareManagerRequest],
) (*connect.Response[codespacev1.DeclareManagerResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Msg.GetProtocolVersion() != 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	s.declared = true
	return connect.NewResponse(&codespacev1.DeclareManagerResponse{
		HeartbeatIntervalMilliseconds:              1000,
		RuntimeMetadataRefreshIntervalMilliseconds: 1000,
		ControlPlaneMaxMessageSizeBytes:            1 << 20,
		GiteaWebUrl:                                "https://gitea.example.com/",
	}), nil
}

func (s *appE2EManagerService) FetchOperations(
	_ context.Context,
	req *connect.Request[codespacev1.FetchOperationsRequest],
) (*connect.Response[codespacev1.FetchOperationsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Msg.GetProtocolVersion() != 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	s.fetched = true
	operation := s.nextOperationLocked()
	if operation == nil {
		return connect.NewResponse(&codespacev1.FetchOperationsResponse{}), nil
	}
	return connect.NewResponse(&codespacev1.FetchOperationsResponse{
		Operations: []*codespacev1.OperationPayload{completeAppE2ECreatePayload(operation)},
	}), nil
}

func completeAppE2ECreatePayload(operation *codespacev1.OperationPayload) *codespacev1.OperationPayload {
	payload := operation.GetCreate()
	if payload == nil {
		return operation
	}
	if payload.UserIdentity == nil {
		payload.UserIdentity = &codespacev1.CodespaceUserIdentity{
			UserId:       101,
			Username:     "e2e-user",
			DisplayName:  "E2E User",
			GitUserName:  "e2e-user",
			GitUserEmail: "e2e-user@example.com",
		}
	}
	if payload.RepositoryConfig == nil {
		payload.RepositoryConfig = &codespacev1.RepositoryCodespaceConfig{
			Path: ".gitea/codespace.yaml",
		}
	}
	return operation
}

func (s *appE2EManagerService) nextOperationLocked() *codespacev1.OperationPayload {
	if len(s.operations) > 0 {
		if s.operationIndex >= len(s.operations) || s.operationIndex > len(s.statuses) {
			return nil
		}
		operation := s.operations[s.operationIndex]
		s.operationIndex++
		return operation
	}
	if s.operation == nil || s.operationIndex > 0 {
		return nil
	}
	s.operationIndex++
	return s.operation
}

func (s *appE2EManagerService) ReportInstances(
	_ context.Context,
	req *connect.Request[codespacev1.ReportInstancesRequest],
) (*connect.Response[codespacev1.ReportInstancesResponse], error) {
	results := make([]*codespacev1.RuntimeInstanceResult, 0, len(req.Msg.GetInstances()))
	for _, instance := range req.Msg.GetInstances() {
		results = append(results, &codespacev1.RuntimeInstanceResult{CodespaceUuid: instance.GetCodespaceUuid()})
	}
	return connect.NewResponse(&codespacev1.ReportInstancesResponse{Results: results}), nil
}

func (s *appE2EManagerService) RequestGiteaToken(
	context.Context,
	*connect.Request[codespacev1.RequestGiteaTokenRequest],
) (*connect.Response[codespacev1.RequestGiteaTokenResponse], error) {
	return connect.NewResponse(&codespacev1.RequestGiteaTokenResponse{
		Token:     "gcs_test",
		ServerUrl: "https://gitea.example.com/",
	}), nil
}

func (s *appE2EManagerService) ReportRuntimeMetadata(
	_ context.Context,
	req *connect.Request[codespacev1.ReportRuntimeMetadataRequest],
) (*connect.Response[codespacev1.ReportRuntimeMetadataResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Msg.GetMetadataGeneration() <= 0 || req.Msg.GetMetadata() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	s.metadata = true
	return connect.NewResponse(&codespacev1.ReportRuntimeMetadataResponse{}), nil
}

func (s *appE2EManagerService) UpdateLog(
	_ context.Context,
	req *connect.Request[codespacev1.UpdateLogRequest],
) (*connect.Response[codespacev1.UpdateLogResponse], error) {
	s.mu.Lock()
	for _, line := range req.Msg.GetLines() {
		s.logs = append(s.logs, line.GetMessage())
	}
	s.mu.Unlock()
	return connect.NewResponse(&codespacev1.UpdateLogResponse{NextOffset: req.Msg.GetOffset() + 1}), nil
}

func (s *appE2EManagerService) FinalizeOperation(
	_ context.Context,
	req *connect.Request[codespacev1.FinalizeOperationRequest],
) (*connect.Response[codespacev1.FinalizeOperationResponse], error) {
	s.mu.Lock()
	s.status = req.Msg.GetFinal().GetStatus()
	s.statuses = append(s.statuses, s.status)
	finalized := s.finalized
	s.mu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		select {
		case finalized <- struct{}{}:
		default:
		}
	}()
	return connect.NewResponse(&codespacev1.FinalizeOperationResponse{
		Outcome: &codespacev1.FinalizeOperationResponse_FinalAccepted{
			FinalAccepted: &codespacev1.FinalAccepted{},
		},
	}), nil
}

func (s *appE2EManagerService) sawDeclare() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.declared
}

func (s *appE2EManagerService) sawFetch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetched
}

func (s *appE2EManagerService) sawMetadata() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metadata
}

func (s *appE2EManagerService) finalStatus() codespacev1.FinalStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *appE2EManagerService) finalStatuses() []codespacev1.FinalStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]codespacev1.FinalStatus(nil), s.statuses...)
}

func (s *appE2EManagerService) operationLog() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.logs, "\n")
}

func waitAppE2EFinalized(t *testing.T, finalized <-chan struct{}) {
	t.Helper()
	waitAppE2EFinalizedCount(t, finalized, 1, 2*time.Second)
}

func waitAppE2EFinalizedCount(t *testing.T, finalized <-chan struct{}, count int, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for i := 0; i < count; i++ {
		select {
		case <-finalized:
		case <-timer.C:
			t.Fatalf("operation finalization count = %d, want %d", i, count)
		}
	}
}

func splitTestHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	hostPort := strings.TrimPrefix(strings.TrimPrefix(raw, "http://"), "tcp://")
	host, portText, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("split address %q: %v", raw, err)
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
		t.Fatalf("parse port %q: %v", portText, err)
	}
	return host, port
}
