// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/pkg/sftp"
)

func TestIncusE2EConnectsToDefaultServer(t *testing.T) {
	requireIncusE2E(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	provisioner, err := NewIncus(incusE2EConfig(1, "connect"))
	if err != nil {
		if strings.Contains(err.Error(), "connect incus") {
			t.Skipf("default Incus connection is unavailable: %v", err)
		}
		t.Fatalf("connect default Incus server: %v", err)
	}
	if _, err := provisioner.ListInstances(ctx); err != nil {
		t.Fatalf("list default Incus instances: %v", err)
	}
}

func TestIncusE2EManagedProjectResources(t *testing.T) {
	requireIncusE2E(t)

	suffix := time.Now().UnixNano()
	projectName := fmt.Sprintf("gitea-codespace-e2e-project-%d", suffix)
	networkName := fmt.Sprintf("cse2e%09x", suffix&0xfffffffff)
	config := incusE2EConfig(suffix, fmt.Sprintf("managed-project-%d", suffix))
	config.Project = projectName
	config.ProjectManage = true
	config.StoragePool = incusE2EEnvDefault("CODESPACE_E2E_INCUS_STORAGE_POOL", "default")
	config.NetworkName = networkName
	config.NetworkManage = true

	baseClient, err := connectIncusBase(config)
	if err != nil {
		skipOrFailIncusE2E(t, "Incus E2E connection is unavailable", err)
	}
	defer cleanupIncusE2EManagedProject(t, baseClient, projectName, networkName)

	provisioner, err := NewIncus(config)
	if err != nil {
		skipOrFailIncusE2E(t, "Incus E2E managed project cannot be prepared", err)
	}
	project, _, err := baseClient.GetProject(projectName)
	if err != nil {
		t.Fatalf("get managed project: %v", err)
	}
	for _, feature := range []string{"features.profiles", "features.storage.volumes"} {
		if !projectFeatureEnabled(project.Config, feature) {
			t.Fatalf("managed project feature %s = %q", feature, project.Config[feature])
		}
	}
	if projectFeatureEnabled(project.Config, "features.networks") {
		t.Fatalf("managed project feature features.networks = %q", project.Config["features.networks"])
	}
	defaultClient := withProject(baseClient, api.ProjectDefaultName)
	network, _, err := defaultClient.GetNetwork(networkName)
	if err != nil {
		t.Fatalf("get managed network: %v", err)
	}
	if network.Type != "bridge" || !network.Managed {
		t.Fatalf("managed network = %#v", network)
	}
	if network.Config["ipv4.dhcp"] != "true" {
		t.Fatalf("managed network ipv4.dhcp = %q", network.Config["ipv4.dhcp"])
	}
	profile, _, err := provisioner.client.GetProfile("default")
	if err != nil {
		t.Fatalf("get managed default profile: %v", err)
	}
	if !profileHasManagedDevices(profile, config.StoragePool, networkName) {
		t.Fatalf("managed default profile devices = %#v", profile.Devices)
	}
}

func TestIncusE2ECreateStopDeleteInstance(t *testing.T) {
	requireIncusE2E(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	config := incusE2EConfig(time.Now().UnixNano(), runID)
	provisioner, err := NewIncus(config)
	if err != nil {
		skipOrFailIncusE2E(t, "Incus E2E connection is unavailable", err)
	}
	if missing, err := incusE2EDeploymentMissing(provisioner, config.RuntimeEnvironments["e2e"].Image); err != nil {
		skipOrFailIncusE2E(t, "Incus E2E deployment cannot be inspected", err)
	} else if len(missing) > 0 {
		skipOrFailIncusE2E(t, "Incus E2E deployment is missing requirements", fmt.Errorf("%s", strings.Join(missing, "; ")))
	}

	suffix := time.Now().UnixNano()
	codespaceUUID := fmt.Sprintf("e2e-%d", suffix)
	instanceName := fmt.Sprintf("gitea-codespace-e2e-%d", suffix)
	spec := InstanceSpec{
		CodespaceUUID:  codespaceUUID,
		Name:           instanceName,
		RepoFullName:   "owner/repo",
		EnvironmentTag: "e2e",
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := cleanupIncusE2EInstance(cleanupCtx, provisioner, instanceName, codespaceUUID, runID); err != nil {
			t.Logf("cleanup e2e instance %s: %v", instanceName, err)
		}
	}()

	instance, err := provisioner.CreateOrStart(ctx, spec)
	if err != nil {
		skipOrFailIncusE2E(t, "default Incus server cannot create the test instance", err)
	}
	if instance.CodespaceUUID != codespaceUUID ||
		instance.Name != instanceName ||
		instance.RuntimeState != RuntimeStateRunning ||
		instance.CommunicationHost == "" {
		t.Fatalf("created instance = %#v", instance)
	}
	assertIncusE2EInstanceRunID(t, ctx, provisioner, instanceName, codespaceUUID, runID)
	assertIncusE2EEnvironmentResources(t, ctx, provisioner, instanceName, config.RuntimeEnvironments["e2e"])
	assertIncusE2EInstanceState(t, ctx, provisioner, codespaceUUID, RuntimeStateRunning)
	assertIncusE2EWorkspaceSFTP(t, ctx, provisioner, instance.Name, instance.Workdir)
	assertIncusE2EWorkspaceAccess(t, ctx, provisioner, instance.Name, instance.Workdir)

	if err := provisioner.Stop(ctx, instanceName); err != nil {
		t.Fatalf("stop e2e instance: %v", err)
	}
	assertIncusE2EInstanceState(t, ctx, provisioner, codespaceUUID, RuntimeStateStopped)

	resumed, err := provisioner.StartExisting(ctx, spec)
	if err != nil {
		t.Fatalf("resume e2e instance: %v", err)
	}
	if resumed.Name != instanceName || resumed.CodespaceUUID != codespaceUUID || resumed.RuntimeState != RuntimeStateRunning {
		t.Fatalf("resumed instance = %#v", resumed)
	}

	if err := provisioner.Stop(ctx, instanceName); err != nil {
		t.Fatalf("stop resumed e2e instance: %v", err)
	}
	assertIncusE2EInstanceState(t, ctx, provisioner, codespaceUUID, RuntimeStateStopped)

	if err := provisioner.Delete(ctx, instanceName); err != nil {
		t.Fatalf("delete e2e instance: %v", err)
	}
	assertIncusE2EInstanceAbsent(t, ctx, provisioner, codespaceUUID)
}

func TestIncusE2EBuiltinLifecycle(t *testing.T) {
	requireIncusE2E(t)
	if !envBool("CODESPACE_E2E_INCUS_BUILTIN_LIFECYCLE") {
		t.Skip("Incus builtin lifecycle E2E is disabled; run make test-e2e-builtin-required to enable it")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	config := incusE2EConfig(time.Now().UnixNano(), runID)
	provisioner, err := NewIncus(config)
	if err != nil {
		skipOrFailIncusE2E(t, "Incus E2E connection is unavailable", err)
	}
	if missing, err := incusE2EDeploymentMissing(provisioner, config.RuntimeEnvironments["e2e"].Image); err != nil {
		skipOrFailIncusE2E(t, "Incus E2E deployment cannot be inspected", err)
	} else if len(missing) > 0 {
		skipOrFailIncusE2E(t, "Incus E2E deployment is missing requirements", fmt.Errorf("%s", strings.Join(missing, "; ")))
	}

	suffix := time.Now().UnixNano()
	codespaceUUID := fmt.Sprintf("e2e-lifecycle-%d", suffix)
	instanceName := fmt.Sprintf("gitea-codespace-e2e-lifecycle-%d", suffix)
	spec := InstanceSpec{
		CodespaceUUID:  codespaceUUID,
		Name:           instanceName,
		RepoFullName:   "octocat/Hello-World",
		EnvironmentTag: "e2e",
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := cleanupIncusE2EInstance(cleanupCtx, provisioner, instanceName, codespaceUUID, runID); err != nil {
			t.Logf("cleanup e2e lifecycle instance %s: %v", instanceName, err)
		}
	}()

	instance, err := provisioner.CreateOrStart(ctx, spec)
	if err != nil {
		skipOrFailIncusE2E(t, "Incus E2E server cannot create the lifecycle test instance", err)
	}
	assertIncusE2EInstanceRunID(t, ctx, provisioner, instanceName, codespaceUUID, runID)
	request := incusE2ELifecycleRequest(codespaceUUID, instanceName, instance.Workdir, ScriptOperationCreate)
	workspace := runIncusE2ELifecycleStart(t, ctx, provisioner, instance, request)

	if _, err := provisioner.StopRuntime(ctx, instanceName, incusE2ELifecycleRequest(codespaceUUID, instanceName, workspace, ScriptOperationStop)); err != nil {
		t.Fatalf("stop e2e lifecycle runtime: %v", err)
	}
	if err := provisioner.Stop(ctx, instanceName); err != nil {
		t.Fatalf("stop e2e lifecycle instance: %v", err)
	}
	assertIncusE2EInstanceState(t, ctx, provisioner, codespaceUUID, RuntimeStateStopped)

	resumed, err := provisioner.StartExisting(ctx, spec)
	if err != nil {
		t.Fatalf("resume e2e lifecycle instance: %v", err)
	}
	resumeRequest := incusE2ELifecycleRequest(codespaceUUID, instanceName, workspace, ScriptOperationResume)
	runIncusE2ELifecycleResume(t, ctx, provisioner, resumed, resumeRequest)

	if _, err := provisioner.StopRuntime(ctx, instanceName, incusE2ELifecycleRequest(codespaceUUID, instanceName, workspace, ScriptOperationStop)); err != nil {
		t.Fatalf("stop resumed e2e lifecycle runtime: %v", err)
	}
	if err := provisioner.Stop(ctx, instanceName); err != nil {
		t.Fatalf("stop resumed e2e lifecycle instance: %v", err)
	}
	if err := provisioner.Delete(ctx, instanceName); err != nil {
		t.Fatalf("delete e2e lifecycle instance: %v", err)
	}
	assertIncusE2EInstanceAbsent(t, ctx, provisioner, codespaceUUID)
}

func runIncusE2ELifecycleStart(t *testing.T, ctx context.Context, provisioner *IncusProvisioner, instance *Instance, request LifecycleRequest) string {
	t.Helper()
	logs := &recordingLifecycleLogSink{}
	request.LogSink = logs
	if err := provisioner.SeedRuntimeCredentials(ctx, instance.Name, incusE2ERuntimeCredentialSeed(request.CodespaceUUID, request.GiteaToken)); err != nil {
		t.Fatalf("seed e2e lifecycle credentials: %v", err)
	}
	identity, err := provisioner.InitializeSystem(ctx, instance.Name, request)
	if err != nil {
		t.Fatalf("initialize e2e lifecycle system: %v\nlifecycle log:\n%s", err, strings.Join(logs.lines, "\n"))
	}
	status, err := provisioner.CheckCredentials(ctx, instance.Name)
	if err != nil {
		t.Fatalf("check e2e lifecycle credentials: %v", err)
	}
	if !status.GiteaTokenPresent {
		t.Fatalf("credential status = %#v", status)
	}
	workspace := identity.SharedEnv["CODESPACE_WORKSPACE_DIR"]
	if !filepath.IsAbs(workspace) {
		t.Fatalf("create workspace = %q", workspace)
	}
	assertIncusE2EWorkspaceAccess(t, ctx, provisioner, instance.Name, workspace)
	gitStatus, err := provisioner.CheckWorkspaceGit(ctx, instance.Name, workspace)
	if err != nil {
		t.Fatalf("check e2e lifecycle workspace git: %v", err)
	}
	if gitStatus.OriginURL == "" || !gitStatus.CredentialConfigured {
		t.Fatalf("workspace git status = %#v", gitStatus)
	}
	startRequest := request
	startRequest.Workdir = workspace
	result, err := provisioner.StartRuntime(ctx, instance.Name, startRequest)
	if err != nil {
		t.Fatalf("start e2e lifecycle runtime: %v\nlifecycle log:\n%s", err, strings.Join(logs.lines, "\n"))
	}
	assertIncusE2EWorkspaceCommand(t, ctx, provisioner, instance.Name, result)
	return workspace
}

func runIncusE2ELifecycleResume(t *testing.T, ctx context.Context, provisioner *IncusProvisioner, instance *Instance, request LifecycleRequest) {
	t.Helper()
	if err := provisioner.SeedRuntimeCredentials(ctx, instance.Name, incusE2ERuntimeCredentialSeed(request.CodespaceUUID, request.GiteaToken)); err != nil {
		t.Fatalf("seed e2e resume credentials: %v", err)
	}
	assertIncusE2EWorkspaceAccess(t, ctx, provisioner, instance.Name, request.Workdir)
	result, err := provisioner.StartRuntime(ctx, instance.Name, request)
	if err != nil {
		t.Fatalf("start e2e resume runtime: %v", err)
	}
	assertIncusE2EWorkspaceCommand(t, ctx, provisioner, instance.Name, result)
}

func assertIncusE2EWorkspaceCommand(t *testing.T, ctx context.Context, provisioner *IncusProvisioner, instanceName string, result LifecycleScriptResult) {
	t.Helper()
	containerID := strings.TrimSpace(result.SharedEnv["CODESPACE_DEVCONTAINER_ID"])
	containerUser := strings.TrimSpace(result.SharedEnv["CODESPACE_DEVCONTAINER_USER"])
	containerWorkdir := strings.TrimSpace(result.SharedEnv["CODESPACE_DEVCONTAINER_WORKDIR"])
	if containerID == "" || containerUser == "" || !filepath.IsAbs(containerWorkdir) {
		t.Fatalf("Dev Container target = %#v", result.SharedEnv)
	}
	if err := provisioner.CheckDevContainer(ctx, instanceName, containerID); err != nil {
		t.Fatalf("check e2e Dev Container: %v", err)
	}
	editorPort, err := strconv.ParseUint(strings.TrimSpace(result.SharedEnv[WorkspaceIDEPortEnv]), 10, 16)
	if err != nil || editorPort != WorkspaceIDEPort {
		t.Fatalf("Web IDE port = %q", result.SharedEnv[WorkspaceIDEPortEnv])
	}
	if err := provisioner.CheckWorkspaceIDE(ctx, instanceName, uint32(editorPort)); err != nil {
		t.Fatalf("check e2e Web IDE: %v", err)
	}
	session, err := provisioner.OpenWorkspaceCommand(ctx, WorkspaceCommandRequest{
		InstanceName:     instanceName,
		ContainerID:      containerID,
		ContainerUser:    containerUser,
		ContainerWorkdir: containerWorkdir,
		Command:          "pwd",
	})
	if err != nil {
		t.Fatalf("open e2e Dev Container command: %v", err)
	}
	defer session.Close()
	output, err := io.ReadAll(session.Stdout())
	if err != nil {
		t.Fatalf("read e2e Dev Container command: %v", err)
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("wait e2e Dev Container command: %v", err)
	}
	if strings.TrimSpace(string(output)) != containerWorkdir {
		t.Fatalf("Dev Container pwd = %q, want %q", strings.TrimSpace(string(output)), containerWorkdir)
	}
}

func incusE2ERuntimeCredentialSeed(codespaceUUID, token string) RuntimeCredentialSeedRequest {
	return RuntimeCredentialSeedRequest{
		CodespaceUUID:    codespaceUUID,
		GiteaToken:       token,
		GitSSHPrivateKey: []byte("-----BEGIN OPENSSH PRIVATE KEY-----\ne2e\n-----END OPENSSH PRIVATE KEY-----\n"),
		GitSSHPublicKey:  []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE2ETestKey gitea-codespace\n"),
		GitSSHKnownHosts: []string{"gitea.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE2ETestHostKey"},
	}
}

func incusE2ELifecycleRequest(codespaceUUID, instanceName, workdir string, operation ScriptOperation) LifecycleRequest {
	return LifecycleRequest{
		CodespaceUUID:      codespaceUUID,
		CodespaceName:      instanceName,
		CodespaceOwnerName: "e2e",
		UserID:             1,
		UserName:           "e2e",
		GitUserName:        "Codespace E2E",
		GitUserEmail:       "codespace-e2e@example.com",
		RuntimeUserName:    "e2e",
		GiteaToken:         "gcs_e2e",
		ServerURL:          "https://gitea.example.com/",
		RepoCloneHTTPURL:   "https://github.com/octocat/Hello-World.git",
		RepoFullName:       "octocat/Hello-World",
		RepoName:           "Hello-World",
		StartRef:           "",
		CommitSHA:          "",
		Workdir:            workdir,
		EnvironmentTag:     "e2e",
		GitProtocol:        "http",
		DevContainer: DevContainerConfiguration{
			Source:       DevContainerSourcePlatformDefault,
			DefaultImage: "mcr.microsoft.com/devcontainers/base:ubuntu",
		},
		Operation: operation,
	}
}

func assertIncusE2EInstanceState(t *testing.T, ctx context.Context, provisioner *IncusProvisioner, codespaceUUID string, state RuntimeState) {
	t.Helper()
	instances, err := provisioner.ListInstances(ctx)
	if err != nil {
		t.Fatalf("list e2e instances: %v", err)
	}
	for _, instance := range instances {
		if instance != nil && instance.CodespaceUUID == codespaceUUID {
			if instance.RuntimeState != state {
				t.Fatalf("instance state = %s, want %s", instance.RuntimeState, state)
			}
			return
		}
	}
	t.Fatalf("e2e instance %s not found", codespaceUUID)
}

func assertIncusE2EEnvironmentResources(t *testing.T, ctx context.Context, provisioner *IncusProvisioner, instanceName string, environment IncusEnvironmentConfig) {
	t.Helper()
	instance, _, err := provisioner.client.GetInstance(instanceName)
	if err != nil {
		t.Fatalf("get e2e instance resources: %v", err)
	}
	expectedType, err := normalizeIncusInstanceType(environment.InstanceType)
	if err != nil {
		t.Fatalf("normalize e2e instance type: %v", err)
	}
	if instance.Type != string(expectedType) {
		t.Fatalf("instance type = %s, want %s", instance.Type, expectedType)
	}
	if instance.Config["limits.cpu"] != fmt.Sprintf("%d", environment.CPU) {
		t.Fatalf("limits.cpu = %q, want %d", instance.Config["limits.cpu"], environment.CPU)
	}
	if instance.Config["limits.memory"] != environment.MemoryLimit {
		t.Fatalf("limits.memory = %q, want %q", instance.Config["limits.memory"], environment.MemoryLimit)
	}
	for _, device := range instance.Devices {
		if device["type"] != "disk" || device["path"] != "/" {
			continue
		}
		if device["size"] != environment.RootDiskSize {
			t.Fatalf("root disk size = %q, want %q", device["size"], environment.RootDiskSize)
		}
		if strings.TrimSpace(device["pool"]) == "" {
			t.Fatalf("root disk pool is empty: %#v", device)
		}
		return
	}
	t.Fatalf("instance %s has no instance-level root disk device", instanceName)
}

func assertIncusE2EWorkspaceSFTP(t *testing.T, ctx context.Context, provisioner *IncusProvisioner, instanceName, workdir string) {
	t.Helper()
	const workspaceUIDGID = 1000
	if err := provisioner.execScript(ctx, instanceName, `
set -eu
if ! getent group 1000 >/dev/null; then
	groupadd -g 1000 codespace-e2e
fi
if ! getent passwd 1000 >/dev/null; then
	useradd -u 1000 -g 1000 -m -s /bin/bash codespace-e2e
fi
mkdir -p -- "$CODESPACE_E2E_WORKDIR"
chown 1000:1000 -- "$CODESPACE_E2E_WORKDIR"
`, map[string]string{
		"CODESPACE_E2E_WORKDIR": workdir,
	}, "/"); err != nil {
		t.Fatalf("prepare e2e workspace directory for sftp: %v", err)
	}
	conn, err := provisioner.OpenWorkspaceSFTP(ctx, WorkspaceSFTPRequest{
		InstanceName: instanceName,
		Workdir:      workdir,
		User:         workspaceUIDGID,
		Group:        workspaceUIDGID,
	})
	if err != nil {
		t.Fatalf("open e2e workspace sftp: %v", err)
	}
	client, err := sftp.NewClientPipe(conn, conn)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("create e2e workspace sftp client: %v", err)
	}
	defer client.Close()
	defer conn.Close()
	currentDirectory, err := client.Getwd()
	if err != nil {
		t.Fatalf("get e2e sftp directory: %v", err)
	}
	if currentDirectory != workdir {
		t.Fatalf("e2e sftp directory = %q, want %q", currentDirectory, workdir)
	}

	file, err := client.Create("sftp-e2e.txt")
	if err != nil {
		t.Fatalf("create e2e sftp file: %v", err)
	}
	defer client.Remove(path.Join(workdir, "sftp-e2e.txt"))
	if _, err := file.Write([]byte("incus sftp ready")); err != nil {
		_ = file.Close()
		t.Fatalf("write e2e sftp file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close e2e sftp file: %v", err)
	}
	opened, err := client.Open("sftp-e2e.txt")
	if err != nil {
		t.Fatalf("open e2e sftp file: %v", err)
	}
	content, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil {
		t.Fatalf("read e2e sftp file: %v", err)
	}
	if string(content) != "incus sftp ready" {
		t.Fatalf("e2e sftp content = %q", content)
	}
	rootFile := "/tmp/gitea-codespace-sftp-e2e.txt"
	outside, err := client.Create(rootFile)
	if err != nil {
		t.Fatalf("create e2e sftp file outside workspace: %v", err)
	}
	if err := outside.Close(); err != nil {
		t.Fatalf("close e2e sftp file outside workspace: %v", err)
	}
	defer client.Remove(rootFile)
	if err := provisioner.execScript(ctx, instanceName, `
set -eu
test "$(stat -c '%u:%g' -- "$CODESPACE_E2E_FILE")" = "1000:1000"
`, map[string]string{
		"CODESPACE_E2E_FILE": path.Join(workdir, "sftp-e2e.txt"),
	}, "/"); err != nil {
		t.Fatalf("verify e2e sftp file ownership: %v", err)
	}
}

func assertIncusE2EWorkspaceAccess(t *testing.T, ctx context.Context, provisioner *IncusProvisioner, instanceName, workdir string) {
	t.Helper()
	if err := provisioner.CheckWorkspaceAccess(ctx, instanceName, workdir); err != nil {
		t.Fatalf("check e2e workspace access: %v", err)
	}
}

func assertIncusE2EInstanceAbsent(t *testing.T, ctx context.Context, provisioner *IncusProvisioner, codespaceUUID string) {
	t.Helper()
	instances, err := provisioner.ListInstances(ctx)
	if err != nil {
		t.Fatalf("list e2e instances: %v", err)
	}
	for _, instance := range instances {
		if instance != nil && instance.CodespaceUUID == codespaceUUID {
			t.Fatalf("e2e instance still exists: %#v", instance)
		}
	}
}

func assertIncusE2EInstanceRunID(t *testing.T, ctx context.Context, provisioner *IncusProvisioner, instanceName, codespaceUUID, runID string) {
	t.Helper()
	instance, _, err := provisioner.client.GetInstance(instanceName)
	if err != nil {
		t.Fatalf("get e2e instance: %v", err)
	}
	if instance.Config[incusConfigManagerID] != provisioner.managerID ||
		instance.Config[incusConfigCodespaceUUID] != codespaceUUID ||
		instance.Config[incusConfigE2ERunID] != runID {
		t.Fatalf("e2e instance config = %#v", instance.Config)
	}
}

func cleanupIncusE2EInstance(ctx context.Context, provisioner *IncusProvisioner, instanceName, codespaceUUID, runID string) error {
	instance, _, err := provisioner.client.GetInstance(instanceName)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("get e2e cleanup instance %s: %w", instanceName, err)
	}
	if instance.Config[incusConfigManagerID] != provisioner.managerID ||
		instance.Config[incusConfigCodespaceUUID] != codespaceUUID ||
		instance.Config[incusConfigE2ERunID] != runID {
		return fmt.Errorf("refuse cleanup for instance %s with config %#v", instanceName, instance.Config)
	}
	return provisioner.Delete(ctx, instanceName)
}

func cleanupIncusE2EManagedProject(t *testing.T, baseClient incus.InstanceServer, projectName, networkName string) {
	t.Helper()
	if baseClient == nil {
		return
	}
	defaultClient := withProject(baseClient, api.ProjectDefaultName)
	if err := defaultClient.DeleteNetwork(networkName); err != nil && !isNotFoundError(err) {
		t.Logf("cleanup managed network %s: %v", networkName, err)
	}
	if err := baseClient.DeleteProject(projectName); err != nil && !isNotFoundError(err) {
		t.Logf("cleanup managed project %s: %v", projectName, err)
	}
}

func incusE2EConfig(managerID int64, runID string) IncusConfig {
	image := strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_IMAGE"))
	if image == "" {
		image = defaultIncusImage
	}
	return IncusConfig{
		ManagerID:   managerID,
		Remote:      strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_REMOTE")),
		UnixSocket:  strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_UNIX_SOCKET")),
		Project:     strings.TrimSpace(os.Getenv("CODESPACE_E2E_INCUS_PROJECT")),
		NetworkName: incusE2EEnvDefault("CODESPACE_E2E_INCUS_NETWORK", "csnet"),
		RuntimeEnvironments: map[string]IncusEnvironmentConfig{
			"e2e": {
				Image:        image,
				InstanceType: incusE2EEnvDefault("CODESPACE_E2E_INCUS_INSTANCE_TYPE", "container"),
				CPU:          1,
				MemoryLimit:  "1GiB",
				RootDiskSize: incusE2EEnvDefault("CODESPACE_E2E_INCUS_ROOT_DISK_SIZE", "10GiB"),
				Profiles:     incusE2EProfiles(),
			},
		},
		ExtraConfig: map[string]string{
			incusConfigE2ERunID: runID,
		},
	}
}

func splitIncusE2EList(value string) []string {
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

func incusE2EEnvDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func incusE2EProfiles() []string {
	profiles := splitIncusE2EList(os.Getenv("CODESPACE_E2E_INCUS_PROFILES"))
	if len(profiles) == 0 {
		return []string{"default"}
	}
	return profiles
}

func incusE2EDeploymentMissing(provisioner *IncusProvisioner, image string) ([]string, error) {
	if provisioner == nil {
		return []string{"incus provisioner is nil"}, nil
	}
	missing := []string{}
	environment := provisioner.environments["e2e"]
	if imageMissing, err := incusE2EImageMissing(provisioner, image); err != nil {
		return nil, err
	} else if imageMissing != "" {
		missing = append(missing, imageMissing)
	}
	hasRootDisk := false
	hasNetworkNIC := false
	for _, profileName := range environment.profiles {
		profile, _, err := provisioner.client.GetProfile(profileName)
		if err != nil {
			return nil, fmt.Errorf("get profile %s: %w", profileName, err)
		}
		for _, device := range profile.Devices {
			switch device["type"] {
			case "disk":
				if device["path"] == "/" && device["pool"] != "" {
					hasRootDisk = true
					if _, _, err := provisioner.client.GetStoragePool(device["pool"]); err != nil {
						if isNotFoundError(err) {
							missing = append(missing, fmt.Sprintf("root disk storage pool %q is missing", device["pool"]))
							continue
						}
						return nil, fmt.Errorf("get storage pool %s: %w", device["pool"], err)
					}
				}
			case "nic":
				networkName := strings.TrimSpace(device["network"])
				if networkName == "" {
					networkName = strings.TrimSpace(device["parent"])
				}
				if networkName != provisioner.networkName {
					continue
				}
				hasNetworkNIC = true
				if missingNetwork, err := incusE2ENetworkMissing(provisioner, device); err != nil {
					return nil, err
				} else if missingNetwork != "" {
					missing = append(missing, missingNetwork)
				}
			}
		}
	}
	if !hasRootDisk {
		missing = append(missing, fmt.Sprintf("profiles %s do not define a root disk", strings.Join(environment.profiles, ",")))
	}
	if !hasNetworkNIC {
		missing = append(missing, fmt.Sprintf("profiles %s do not define a NIC connected to network %s", strings.Join(environment.profiles, ","), provisioner.networkName))
	}
	return missing, nil
}

func incusE2EImageMissing(provisioner *IncusProvisioner, image string) (string, error) {
	source := api.InstanceSource{
		Alias:       incusImageAlias(image),
		Fingerprint: incusImageFingerprint(image),
		Server:      imageServerForAlias(image),
	}
	if source.Server != "" {
		return "", nil
	}
	if source.Fingerprint != "" {
		if _, _, err := provisioner.client.GetImage(source.Fingerprint); err != nil {
			if isNotFoundError(err) {
				return fmt.Sprintf("local image fingerprint %q is missing", source.Fingerprint), nil
			}
			return "", fmt.Errorf("get local image %s: %w", source.Fingerprint, err)
		}
		return "", nil
	}
	if source.Alias != "" {
		if _, _, err := provisioner.client.GetImageAlias(source.Alias); err != nil {
			if isNotFoundError(err) {
				return fmt.Sprintf("local image alias %q is missing", source.Alias), nil
			}
			return "", fmt.Errorf("get local image alias %s: %w", source.Alias, err)
		}
	}
	return "", nil
}

func incusE2ENetworkMissing(provisioner *IncusProvisioner, device map[string]string) (string, error) {
	networkName := strings.TrimSpace(device["network"])
	if networkName == "" {
		networkName = strings.TrimSpace(device["parent"])
	}
	if networkName == "" {
		return "", nil
	}
	network, _, err := provisioner.client.GetNetwork(networkName)
	if err != nil {
		if isNotFoundError(err) {
			return fmt.Sprintf("nic network %q is missing", networkName), nil
		}
		return "", fmt.Errorf("get network %s: %w", networkName, err)
	}
	if !network.Managed {
		return fmt.Sprintf("nic network %q is not managed", networkName), nil
	}
	return "", nil
}

func skipOrFailIncusE2E(t *testing.T, message string, err error) {
	t.Helper()
	if envBool("CODESPACE_E2E_REQUIRE_INCUS") {
		t.Fatalf("%s: %v", message, err)
	}
	t.Skipf("%s: %v", message, err)
}

func requireIncusE2E(t *testing.T) {
	t.Helper()
	if !envBool("CODESPACE_E2E_INCUS") {
		t.Skip("Incus E2E is disabled; run make test-e2e to enable it")
	}
}

func envBool(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}
