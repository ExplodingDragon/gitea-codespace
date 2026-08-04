// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v6/shared/api"
	"github.com/pkg/sftp"
)

type recordingLifecycleLogSink struct {
	lines []string
}

func (s *recordingLifecycleLogSink) WriteLifecycleLog(ctx context.Context, message string) error {
	s.lines = append(s.lines, message)
	return nil
}

func TestIncusInstanceFromAPIRequiresManagerOwnership(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{managerID: "7"}
	instance, ok := provisioner.instanceFromAPI(api.Instance{
		Name:   "cs-11111111222243338444",
		Status: "Running",
		InstancePut: api.InstancePut{
			Config: map[string]string{
				incusConfigManagerID:      "7",
				incusConfigCodespaceUUID:  "11111111-2222-4333-8444-555555555555",
				incusConfigEnvironmentTag: "default",
			},
		},
	})
	if !ok {
		t.Fatalf("owned instance was not accepted")
	}
	if instance.CodespaceUUID != "11111111-2222-4333-8444-555555555555" ||
		instance.Name != "cs-11111111222243338444" ||
		instance.RuntimeState != RuntimeStateRunning ||
		instance.EnvironmentTag != "default" {
		t.Fatalf("instance = %#v", instance)
	}
}

func TestIncusInstanceFromAPISkipsOtherManagers(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{managerID: "7"}
	_, ok := provisioner.instanceFromAPI(api.Instance{
		Name:   "cs-11111111222243338444",
		Status: "Running",
		InstancePut: api.InstancePut{
			Config: map[string]string{
				incusConfigManagerID:     "8",
				incusConfigCodespaceUUID: "11111111-2222-4333-8444-555555555555",
			},
		},
	})
	if ok {
		t.Fatalf("instance owned by another manager was accepted")
	}
}

func TestIncusInstanceFromAPISkipsMissingCodespaceUUID(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{managerID: "7"}
	_, ok := provisioner.instanceFromAPI(api.Instance{
		Name:   "cs-11111111222243338444",
		Status: "Running",
		InstancePut: api.InstancePut{
			Config: map[string]string{
				incusConfigManagerID: "7",
			},
		},
	})
	if ok {
		t.Fatalf("instance without codespace uuid was accepted")
	}
}

func TestRuntimeCredentialSeedUsesFixedPathsAndModes(t *testing.T) {
	t.Parallel()
	keyFiles, err := runtimeGitSSHKeySeedFiles(RuntimeGitSSHKeySeedRequest{
		GitSSHPrivateKey: []byte("private-key"),
		GitSSHPublicKey:  []byte("public-key"),
	})
	if err != nil {
		t.Fatalf("runtime git ssh key seed files: %v", err)
	}
	wantKeyFiles := []bootstrapCredentialFile{
		{path: runtimeSeedGitSSHPrivateKey, content: "private-key", mode: runtimeCredentialFileMode},
		{path: runtimeSeedGitSSHPublicKey, content: "public-key", mode: 0o644},
	}
	if !reflect.DeepEqual(keyFiles, wantKeyFiles) {
		t.Fatalf("git ssh key seed files = %#v", keyFiles)
	}

	for _, tc := range []struct {
		name              string
		knownHosts        []string
		knownHostsContent string
	}{
		{name: "with known hosts", knownHosts: []string{"known-hosts"}, knownHostsContent: "known-hosts\n"},
		{name: "without known hosts", knownHostsContent: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := runtimeCredentialSeedFiles(RuntimeCredentialSeedRequest{
				CodespaceUUID:    "codespace-uuid",
				GiteaToken:       "gitea-token",
				GitSSHKnownHosts: tc.knownHosts,
			})
			if err != nil {
				t.Fatalf("runtime credential seed files: %v", err)
			}
			want := []bootstrapCredentialFile{
				{
					path:    runtimeSeedGiteaToken,
					content: "gitea-token",
					mode:    runtimeCredentialFileMode,
				},
				{path: runtimeSeedGitSSHKnownHosts, content: tc.knownHostsContent, mode: runtimeCredentialFileMode},
			}
			if !reflect.DeepEqual(files, want) {
				t.Fatalf("credential seed files = %#v", files)
			}
		})
	}
}

func TestLifecycleLogWriterEmitsCompleteLines(t *testing.T) {
	t.Parallel()

	sink := &recordingLifecycleLogSink{}
	writer := newLifecycleLogWriter(context.Background(), sink)
	if _, err := writer.Write([]byte("alpha\nbeta")); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	if _, err := writer.Write([]byte("\ngamma")); err != nil {
		t.Fatalf("write second chunk: %v", err)
	}
	writer.Flush()

	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(sink.lines, want) {
		t.Fatalf("log lines = %#v", sink.lines)
	}
}

func TestNormalizedBootstrapConfigFillsRuntimeDefaults(t *testing.T) {
	t.Parallel()

	config := normalizedBootstrapConfig(BootstrapConfig{})

	if config.Shell != defaultBootstrapShell {
		t.Fatalf("bootstrap shell = %q", config.Shell)
	}
	if config.HomeDir != defaultBootstrapHomeDir {
		t.Fatalf("bootstrap home dir = %q", config.HomeDir)
	}
	if config.UserName != defaultBootstrapUserName {
		t.Fatalf("bootstrap user name = %q", config.UserName)
	}
}

func TestNormalizeIncusInstanceType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  api.InstanceType
	}{
		{name: "default", want: api.InstanceTypeContainer},
		{name: "container", value: "container", want: api.InstanceTypeContainer},
		{name: "lxc", value: "lxc", want: api.InstanceTypeContainer},
		{name: "virtual machine", value: "virtual-machine", want: api.InstanceTypeVM},
		{name: "vm", value: "vm", want: api.InstanceTypeVM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeIncusInstanceType(tt.value)
			if err != nil {
				t.Fatalf("normalize instance type: %v", err)
			}
			if got != tt.want {
				t.Fatalf("instance type = %q, want %q", got, tt.want)
			}
		})
	}

	if _, err := normalizeIncusInstanceType("serverless"); err == nil {
		t.Fatalf("expected invalid instance type error")
	}
}

func TestNormalizeIncusEnvironmentsRequiresCompleteEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		environment IncusEnvironmentConfig
		want        string
	}{
		{name: "image", environment: IncusEnvironmentConfig{InstanceType: "container", CPU: 1, MemoryLimit: "1GiB", RootDiskSize: "10GiB", Profiles: []string{"default"}}, want: "image"},
		{name: "cpu", environment: IncusEnvironmentConfig{Image: "images:debian/12", InstanceType: "container", MemoryLimit: "1GiB", RootDiskSize: "10GiB", Profiles: []string{"default"}}, want: "cpu"},
		{name: "memory", environment: IncusEnvironmentConfig{Image: "images:debian/12", InstanceType: "container", CPU: 1, RootDiskSize: "10GiB", Profiles: []string{"default"}}, want: "memory"},
		{name: "root disk", environment: IncusEnvironmentConfig{Image: "images:debian/12", InstanceType: "container", CPU: 1, MemoryLimit: "1GiB", Profiles: []string{"default"}}, want: "resources.root_disk"},
		{name: "profiles", environment: IncusEnvironmentConfig{Image: "images:debian/12", InstanceType: "container", CPU: 1, MemoryLimit: "1GiB", RootDiskSize: "10GiB"}, want: "profiles"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := normalizeIncusEnvironments(IncusConfig{
				RuntimeEnvironments: map[string]IncusEnvironmentConfig{"default": tt.environment},
			})
			if err == nil {
				t.Fatalf("expected environment validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("environment validation error = %v", err)
			}
		})
	}
}

func TestIncusCreateRequestUsesEnvironmentResources(t *testing.T) {
	t.Parallel()

	request := incusCreateRequest(InstanceSpec{
		CodespaceUUID:  "11111111-1111-4111-8111-111111111111",
		Name:           "cs-test",
		EnvironmentTag: "default",
	}, incusEnvironment{
		image:        "images:debian/12",
		instanceType: api.InstanceTypeContainer,
		cpu:          2,
		memoryLimit:  "1GiB",
		rootDiskSize: "10GiB",
		profiles:     []string{"default"},
	}, "root", map[string]string{
		"type": "disk",
		"path": "/",
		"pool": "default",
	}, map[string]string{
		incusConfigManagerID:      "7",
		incusConfigCodespaceUUID:  "11111111-1111-4111-8111-111111111111",
		incusConfigEnvironmentTag: "default",
	})

	if request.Type != api.InstanceTypeContainer {
		t.Fatalf("instance type = %q", request.Type)
	}
	if request.Config["limits.cpu"] != "2" || request.Config["limits.memory"] != "1GiB" {
		t.Fatalf("instance config = %#v", request.Config)
	}
	if request.Config["security.nesting"] != "true" {
		t.Fatalf("container nesting config = %#v", request.Config)
	}
	if request.Devices["root"]["type"] != "disk" ||
		request.Devices["root"]["path"] != "/" ||
		request.Devices["root"]["pool"] != "default" ||
		request.Devices["root"]["size"] != "10GiB" {
		t.Fatalf("root disk device = %#v", request.Devices["root"])
	}
	if len(request.Profiles) != 1 || request.Profiles[0] != "default" {
		t.Fatalf("profiles = %#v", request.Profiles)
	}
}

func TestIncusCreateRequestUsesInstanceSource(t *testing.T) {
	t.Parallel()

	request := incusCreateRequest(InstanceSpec{
		CodespaceUUID:  "11111111-1111-4111-8111-111111111111",
		Name:           "cs-test",
		EnvironmentTag: "default",
	}, incusEnvironment{
		sourceType:    "instance",
		sourceProject: "base-images",
		sourceName:    "dev-environment",
		instanceType:  api.InstanceTypeVM,
		profiles:      []string{"default"},
	}, "", nil, map[string]string{})

	if request.Source.Type != "copy" ||
		request.Source.Source != "dev-environment" ||
		request.Source.Project != "base-images" ||
		!request.Source.InstanceOnly {
		t.Fatalf("instance source = %#v", request.Source)
	}
	if _, ok := request.Config["security.nesting"]; ok {
		t.Fatalf("virtual machine config = %#v", request.Config)
	}
}

func TestProjectFeatureEnabled(t *testing.T) {
	t.Parallel()

	if !projectFeatureEnabled(map[string]string{"features.profiles": "true"}, "features.profiles") {
		t.Fatalf("true project feature was not accepted")
	}
	if !projectFeatureEnabled(map[string]string{"features.profiles": "1"}, "features.profiles") {
		t.Fatalf("numeric project feature was not accepted")
	}
	if projectFeatureEnabled(map[string]string{"features.profiles": "false"}, "features.profiles") {
		t.Fatalf("false project feature was accepted")
	}
}

func TestConfigureDockerDaemonPreservesExistingSettings(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{
		buildCacheRegistry: "http://registry.example.com/codespace",
		registryMirrors: map[string]string{
			"docker.io": "https://docker-cache.example.com",
			"ghcr.io":   "http://registry.example.com/ghcr",
		},
	}
	encoded, changed, err := provisioner.dockerDaemonConfiguration(`{"log-level":"warn","registry-mirrors":["https://existing.example.com"]}`)
	if err != nil || !changed {
		t.Fatalf("build Docker daemon configuration = %v, %v", changed, err)
	}
	var config struct {
		LogLevel           string   `json:"log-level"`
		RegistryMirrors    []string `json:"registry-mirrors"`
		InsecureRegistries []string `json:"insecure-registries"`
	}
	if err := json.Unmarshal([]byte(encoded), &config); err != nil {
		t.Fatalf("decode Docker daemon cache config: %v", err)
	}
	if config.LogLevel != "warn" {
		t.Fatalf("Docker log level = %q", config.LogLevel)
	}
	if len(config.RegistryMirrors) != 2 || config.RegistryMirrors[0] != "https://docker-cache.example.com" || config.RegistryMirrors[1] != "https://existing.example.com" {
		t.Fatalf("Docker registry mirrors = %#v", config.RegistryMirrors)
	}
	if len(config.InsecureRegistries) != 1 || config.InsecureRegistries[0] != "registry.example.com" {
		t.Fatalf("Docker insecure registries = %#v", config.InsecureRegistries)
	}
}

func TestManagedProjectFeaturesShareDefaultNetworks(t *testing.T) {
	t.Parallel()

	config := map[string]string{
		"features.profiles":        "false",
		"features.networks":        "true",
		"features.storage.volumes": "false",
	}
	if !managedProjectFeaturesNeedUpdate(config) {
		t.Fatalf("expected managed project features to require update")
	}
	applyManagedProjectFeatures(config)
	if !projectFeatureEnabled(config, "features.profiles") ||
		projectFeatureEnabled(config, "features.networks") ||
		!projectFeatureEnabled(config, "features.storage.volumes") {
		t.Fatalf("managed project features = %#v", config)
	}
	if managedProjectFeaturesNeedUpdate(config) {
		t.Fatalf("managed project features still require update: %#v", config)
	}
}

func TestProfileHasManagedDevices(t *testing.T) {
	t.Parallel()

	profile := &api.Profile{
		ProfilePut: api.ProfilePut{
			Devices: map[string]map[string]string{
				"root": {"type": "disk", "path": "/", "pool": "default"},
				"eth0": {"type": "nic", "network": "codespace-net"},
			},
		},
	}
	if !profileHasManagedDevices(profile, "default", "codespace-net") {
		t.Fatalf("managed profile devices were not accepted")
	}
	if profileHasManagedDevices(profile, "other", "codespace-net") {
		t.Fatalf("wrong storage pool was accepted")
	}
	if profileHasManagedDevices(profile, "default", "other-net") {
		t.Fatalf("wrong network was accepted")
	}
}

func TestIncusStartupAdmissionUsesProjectInstanceQuota(t *testing.T) {
	t.Parallel()

	available, err := incusEnvironmentCreateAvailable(&api.ProjectState{
		Resources: map[string]api.ProjectStateResource{
			"instances": {Limit: 1, Usage: 1},
		},
	}, incusEnvironment{instanceType: api.InstanceTypeContainer, memoryLimit: "1GiB"})
	if err != nil {
		t.Fatalf("startup admission: %v", err)
	}
	if available {
		t.Fatalf("create should be unavailable when project instance quota is full")
	}
}

func TestIncusStartupAdmissionUsesTypeAndMemoryQuota(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		instance    api.InstanceType
		memoryLimit string
		resources   map[string]api.ProjectStateResource
		wantCreate  bool
	}{
		{
			name:        "container type quota available",
			instance:    api.InstanceTypeContainer,
			memoryLimit: "1GiB",
			resources: map[string]api.ProjectStateResource{
				"containers": {Limit: 2, Usage: 1},
				"memory":     {Limit: 3 << 30, Usage: 1 << 30},
			},
			wantCreate: true,
		},
		{
			name:        "container type quota full",
			instance:    api.InstanceTypeContainer,
			memoryLimit: "1GiB",
			resources: map[string]api.ProjectStateResource{
				"containers": {Limit: 1, Usage: 1},
			},
		},
		{
			name:        "vm type quota full",
			instance:    api.InstanceTypeVM,
			memoryLimit: "1GiB",
			resources: map[string]api.ProjectStateResource{
				"virtual-machines": {Limit: 1, Usage: 1},
			},
		},
		{
			name:        "memory quota full",
			instance:    api.InstanceTypeContainer,
			memoryLimit: "1GiB",
			resources: map[string]api.ProjectStateResource{
				"memory": {Limit: 2 << 30, Usage: 1536 << 20},
			},
		},
		{
			name:     "missing memory limit with project memory quota",
			instance: api.InstanceTypeContainer,
			resources: map[string]api.ProjectStateResource{
				"memory": {Limit: 2 << 30, Usage: 0},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			available, err := incusEnvironmentCreateAvailable(&api.ProjectState{Resources: tt.resources}, incusEnvironment{
				instanceType: tt.instance,
				memoryLimit:  tt.memoryLimit,
			})
			if err != nil {
				t.Fatalf("startup admission: %v", err)
			}
			if available != tt.wantCreate {
				t.Fatalf("create available = %v, want %v", available, tt.wantCreate)
			}
		})
	}
}

func TestIncusStartupAdmissionUsesDiskQuota(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		rootDiskSize string
		resource     api.ProjectStateResource
		wantCreate   bool
	}{
		{
			name:         "disk quota available",
			rootDiskSize: "1GiB",
			resource:     api.ProjectStateResource{Limit: 3 << 30, Usage: 1 << 30},
			wantCreate:   true,
		},
		{
			name:         "disk quota full",
			rootDiskSize: "2GiB",
			resource:     api.ProjectStateResource{Limit: 3 << 30, Usage: 2 << 30},
		},
		{
			name:     "missing root disk with disk quota",
			resource: api.ProjectStateResource{Limit: 3 << 30, Usage: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			available, err := incusEnvironmentCreateAvailable(&api.ProjectState{
				Resources: map[string]api.ProjectStateResource{
					"disk": tt.resource,
				},
			}, incusEnvironment{
				instanceType: api.InstanceTypeContainer,
				memoryLimit:  "1GiB",
				rootDiskSize: tt.rootDiskSize,
			})
			if err != nil {
				t.Fatalf("startup admission: %v", err)
			}
			if available != tt.wantCreate {
				t.Fatalf("create available = %v, want %v", available, tt.wantCreate)
			}
		})
	}
}

func TestIncusStartupAdmissionReturnsEnvironmentsThatFit(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{
		environments: map[string]incusEnvironment{
			"small": {instanceType: api.InstanceTypeContainer, memoryLimit: "512MiB"},
			"large": {instanceType: api.InstanceTypeContainer, memoryLimit: "2GiB"},
		},
	}
	admission, err := provisioner.incusStartupAdmission(&api.ProjectState{
		Resources: map[string]api.ProjectStateResource{
			"memory": {Limit: 2 << 30, Usage: 1 << 30},
		},
	})
	if err != nil {
		t.Fatalf("startup admission: %v", err)
	}
	if !reflect.DeepEqual(admission.CreateTags, []string{"small"}) {
		t.Fatalf("create tags = %v, want [small]", admission.CreateTags)
	}
	if !admission.ResumeAvailable {
		t.Fatalf("resume should remain available")
	}
}

func TestWorkspaceSFTPHandlersUseInstancePaths(t *testing.T) {
	t.Parallel()

	instanceClient, closeInstance := newTestSFTPClient(t, sftp.InMemHandler())
	defer closeInstance()
	if err := instanceClient.Mkdir("/workspaces"); err != nil {
		t.Fatalf("mkdir /workspaces: %v", err)
	}
	if err := instanceClient.Mkdir("/workspaces/repo"); err != nil {
		t.Fatalf("mkdir /workspaces/repo: %v", err)
	}
	workspaceClient, closeWorkspace := newTestSFTPClient(t, workspaceSFTPHandlers(instanceClient, "/workspaces/repo", 0, 0))
	defer closeWorkspace()
	workdir, err := workspaceClient.Getwd()
	if err != nil {
		t.Fatalf("get workspace directory: %v", err)
	}
	if workdir != "/workspaces/repo" {
		t.Fatalf("workspace directory = %q", workdir)
	}

	if err := workspaceClient.Mkdir(path.Join(workdir, "dir")); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	file, err := workspaceClient.Create(path.Join(workdir, "dir/file.txt"))
	if err != nil {
		t.Fatalf("create workspace file: %v", err)
	}
	if _, err := file.Write([]byte("workspace file")); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close workspace file: %v", err)
	}
	instanceFile, err := instanceClient.Open("/workspaces/repo/dir/file.txt")
	if err != nil {
		t.Fatalf("open mapped instance file: %v", err)
	}
	content, err := io.ReadAll(instanceFile)
	_ = instanceFile.Close()
	if err != nil {
		t.Fatalf("read mapped instance file: %v", err)
	}
	if string(content) != "workspace file" {
		t.Fatalf("mapped file content = %q", content)
	}

	escaped, err := workspaceClient.Create("/outside.txt")
	if err != nil {
		t.Fatalf("create instance root file: %v", err)
	}
	_ = escaped.Close()
	if _, err := instanceClient.Stat("/outside.txt"); err != nil {
		t.Fatalf("instance root file is unavailable: %v", err)
	}
}

func newTestSFTPClient(t *testing.T, handlers sftp.Handlers) (*sftp.Client, func()) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := sftp.NewRequestServer(serverConn, handlers, sftp.WithStartDirectory("/"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve()
		_ = serverConn.Close()
	}()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		_ = clientConn.Close()
		t.Fatalf("create test sftp client: %v", err)
	}
	return client, func() {
		_ = client.Close()
		_ = clientConn.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("test sftp server did not stop")
		}
	}
}

func TestIsIncusVMAgentUnavailable(t *testing.T) {
	t.Parallel()

	if !isIncusVMAgentUnavailable(errors.New("VM agent isn't currently running")) {
		t.Fatalf("expected vm agent unavailable")
	}
	if isIncusVMAgentUnavailable(errors.New("network unavailable")) {
		t.Fatalf("unexpected vm agent unavailable")
	}
}

func TestParseBootstrapOutputKeepsLastValueAndIgnoresPredefined(t *testing.T) {
	t.Parallel()

	values, err := parseEnvironmentFile("A=1\nCODESPACE_UUID=ignored\nA=2\nPRIVATE=value=with=equals\n", map[string]struct{}{
		"CODESPACE_UUID": {},
	})
	if err != nil {
		t.Fatalf("parse bootstrap output: %v", err)
	}
	want := map[string]string{
		"A":       "2",
		"PRIVATE": "value=with=equals",
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("bootstrap output = %#v", values)
	}
}

func TestParseBootstrapOutputTrimsTrailingNULPadding(t *testing.T) {
	t.Parallel()

	values, err := parseEnvironmentFile("A=1\nB=2\n\x00\x00", nil)
	if err != nil {
		t.Fatalf("parse bootstrap output: %v", err)
	}
	want := map[string]string{
		"A": "1",
		"B": "2",
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("bootstrap output = %#v", values)
	}
}

func TestParseBootstrapOutputRejectsInvalidContent(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		"NO_EQUALS\n",
		"BAD-NAME=value\n",
		"A=value\x00\n",
		"A=value\n\x00\n",
	} {
		if _, err := parseEnvironmentFile(content, nil); err == nil {
			t.Fatalf("expected error for %q", content)
		}
	}
}

func TestValidateBootstrapResultRequiresDoneAtExpectedStage(t *testing.T) {
	t.Parallel()

	if err := validateBootstrapResult(`{"outcome":"done","stage":"prepare-workspace"}`, "prepare-workspace"); err != nil {
		t.Fatalf("validate result: %v", err)
	}
	recoverable := []string{
		`{"outcome":"recoverable_failed","stage":"prepare-workspace"}`,
		`{"outcome":"unknown","stage":"prepare-workspace"}`,
		`{"outcome":"done","stage":"start-environment"}`,
		`{"outcome":"done","stage":"prepare-workspace","extra":true}`,
	}
	for _, content := range recoverable {
		if err := validateBootstrapResult(content, "prepare-workspace"); err == nil {
			t.Fatalf("expected result error for %s", content)
		} else if !IsRecoverableRuntimeFailure(err) {
			t.Fatalf("expected recoverable result error for %s, got %v", content, err)
		}
	}
	err := validateBootstrapResult(`{"outcome":"unrecoverable_failed","stage":"prepare-workspace"}`, "prepare-workspace")
	var failure *RuntimeFailureError
	if !errors.As(err, &failure) || failure.Kind != RuntimeFailureUnrecoverable {
		t.Fatalf("unrecoverable result error = %#v", err)
	}
}

func TestIncusImageSourceFields(t *testing.T) {
	t.Parallel()

	fingerprint := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name        string
		image       string
		alias       string
		fingerprint string
		server      string
		protocol    string
	}{
		{
			name:     "simplestreams alias",
			image:    "images:debian/12",
			alias:    "debian/12",
			server:   "https://images.linuxcontainers.org",
			protocol: "simplestreams",
		},
		{
			name:  "local alias",
			image: "codespace-test",
			alias: "codespace-test",
		},
		{
			name:        "local fingerprint",
			image:       "local:" + fingerprint,
			fingerprint: fingerprint,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := incusImageAlias(tt.image); got != tt.alias {
				t.Fatalf("alias = %q", got)
			}
			if got := incusImageFingerprint(tt.image); got != tt.fingerprint {
				t.Fatalf("fingerprint = %q", got)
			}
			if got := imageServerForAlias(tt.image); got != tt.server {
				t.Fatalf("server = %q", got)
			}
			if got := imageProtocolForAlias(tt.image); got != tt.protocol {
				t.Fatalf("protocol = %q", got)
			}
		})
	}
}

func TestRuntimeBuildCacheScopeIgnoresRepositoryCommit(t *testing.T) {
	t.Parallel()

	request := LifecycleRequest{
		RepoFullName:   "owner/repo",
		EnvironmentTag: "debian-lxc",
		CommitSHA:      strings.Repeat("a", 40),
		DevContainer: DevContainerConfiguration{
			Source:  DevContainerSourceTemplate,
			Content: `{"image":"debian:12"}`,
		},
	}
	first := RuntimeBuildCacheScope(request, "4.121.0")
	request.CommitSHA = strings.Repeat("c", 40)
	if got := RuntimeBuildCacheScope(request, "4.121.0"); got != first {
		t.Fatalf("cache scope changed after commit-only change: %q / %q", first, got)
	}
	request.DevContainer.Content = `{"image":"debian:13"}`
	if got := RuntimeBuildCacheScope(request, "4.121.0"); got == first {
		t.Fatalf("cache scope did not change after configuration content change")
	}
}

func TestInstanceNetworkDeviceUsesExpandedNICMAC(t *testing.T) {
	t.Parallel()

	instance := &api.Instance{
		InstancePut: api.InstancePut{
			Config: map[string]string{"volatile.runtime0.hwaddr": "00:16:3e:01:02:03"},
		},
		ExpandedDevices: map[string]map[string]string{
			"root":     {"type": "disk", "path": "/"},
			"runtime0": {"type": "nic", "network": "csnet"},
		},
	}
	deviceName, hardwareAddress, err := instanceNetworkDevice(instance, "csnet")
	if err != nil {
		t.Fatalf("resolve instance network device: %v", err)
	}
	if deviceName != "runtime0" || hardwareAddress != "00:16:3e:01:02:03" {
		t.Fatalf("network device = %q %q", deviceName, hardwareAddress)
	}
}

func TestInstanceNetworkDeviceRequiresUniqueTargetNetworkNIC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		devices  map[string]map[string]string
		wantText string
	}{
		{
			name:     "missing",
			devices:  map[string]map[string]string{"eth0": {"type": "nic", "network": "other", "hwaddr": "00:16:3e:01:02:03"}},
			wantText: "no NIC device",
		},
		{
			name: "multiple",
			devices: map[string]map[string]string{
				"eth0": {"type": "nic", "network": "csnet", "hwaddr": "00:16:3e:01:02:03"},
				"eth1": {"type": "nic", "parent": "csnet", "hwaddr": "00:16:3e:01:02:04"},
			},
			wantText: "multiple NIC devices",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			instance := &api.Instance{ExpandedDevices: tt.devices}
			if _, _, err := instanceNetworkDevice(instance, "csnet"); err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("network device error = %v", err)
			}
		})
	}
}

func TestInstanceStateCommunicationHostMatchesMACAcrossGuestNames(t *testing.T) {
	t.Parallel()

	for _, interfaceName := range []string{"eth0", "enp5s0", "workspace0"} {
		t.Run(interfaceName, func(t *testing.T) {
			t.Parallel()
			state := &api.InstanceState{
				Network: map[string]api.InstanceStateNetwork{
					interfaceName: {
						Hwaddr: "00:16:3e:01:02:03",
						Addresses: []api.InstanceStateNetworkAddress{
							{Address: "fe80::1", Scope: "global"},
							{Address: "10.0.0.12", Scope: "global"},
						},
					},
					"other": {
						Hwaddr:    "00:16:3e:01:02:04",
						Addresses: []api.InstanceStateNetworkAddress{{Address: "10.0.1.12", Scope: "global"}},
					},
				},
			}

			host, err := instanceStateCommunicationHost(state, "00:16:3e:01:02:03")
			if err != nil {
				t.Fatalf("resolve communication host: %v", err)
			}
			if host != "10.0.0.12" {
				t.Fatalf("communication host = %q", host)
			}
		})
	}
}

func TestValidateIncusServerAcceptsTrustedNonClusteredProject(t *testing.T) {
	t.Parallel()

	if err := validateIncusServer(&api.Server{
		ServerUntrusted: api.ServerUntrusted{
			Auth: "trusted",
		},
		Environment: api.ServerEnvironment{
			Server:          "incus",
			Project:         "codespace",
			ServerClustered: false,
		},
	}, "codespace"); err != nil {
		t.Fatalf("validate incus server: %v", err)
	}
}

func TestValidateIncusServerRejectsUnsupportedServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		server  *api.Server
		project string
	}{
		{
			name: "untrusted",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "untrusted"},
				Environment:     api.ServerEnvironment{Server: "incus", Project: "codespace"},
			},
			project: "codespace",
		},
		{
			name: "clustered",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "trusted"},
				Environment: api.ServerEnvironment{
					Server:          "incus",
					Project:         "codespace",
					ServerClustered: true,
				},
			},
			project: "codespace",
		},
		{
			name: "wrong project",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "trusted"},
				Environment:     api.ServerEnvironment{Server: "incus", Project: "default"},
			},
			project: "codespace",
		},
		{
			name: "public only",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "trusted", Public: true},
				Environment:     api.ServerEnvironment{Server: "incus", Project: "codespace"},
			},
			project: "codespace",
		},
		{
			name: "not incus",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "trusted"},
				Environment:     api.ServerEnvironment{Server: "lxd", Project: "codespace"},
			},
			project: "codespace",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if err := validateIncusServer(test.server, test.project); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestWorkspaceGitCredentialConfigured(t *testing.T) {
	t.Parallel()

	if !workspaceGitCredentialConfigured(
		"ssh://git@gitea.example.com/owner/repo.git",
		"",
		"",
		"/var/lib/gitea-codespace/bin/gitea-codespace-git-ssh",
	) {
		t.Fatal("Dev Container Git SSH helper was not accepted")
	}
	if !workspaceGitCredentialConfigured(
		"https://gitea.example.com/owner/repo.git",
		"!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential",
		"",
		"",
	) {
		t.Fatal("Dev Container Git HTTP helper was not accepted")
	}
}

func TestBoundedOutputBufferKeepsRecentOutput(t *testing.T) {
	t.Parallel()

	buffer := newBoundedOutputBuffer(5)
	if _, err := buffer.Write([]byte("abc")); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	if _, err := buffer.Write([]byte("defg")); err != nil {
		t.Fatalf("write second chunk: %v", err)
	}

	if got := buffer.String(); !strings.Contains(got, "cdefg") || !strings.Contains(got, "truncated 2 bytes") {
		t.Fatalf("buffer = %q", got)
	}
	if !buffer.Truncated() {
		t.Fatalf("buffer was not marked truncated")
	}
}
