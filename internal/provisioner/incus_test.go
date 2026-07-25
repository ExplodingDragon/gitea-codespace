// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v6/shared/api"
	"github.com/pkg/sftp"
)

func TestIncusInstanceFromAPIRequiresManagerOwnership(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{managerID: "7"}
	instance, ok := provisioner.instanceFromAPI(api.Instance{
		Name:   "cs-11111111222243338444",
		Status: "Running",
		InstancePut: api.InstancePut{
			Config: map[string]string{
				incusConfigManagerID:     "7",
				incusConfigCodespaceUUID: "11111111-2222-4333-8444-555555555555",
				incusConfigTag:           "default",
			},
		},
	})
	if !ok {
		t.Fatalf("owned instance was not accepted")
	}
	if instance.CodespaceUUID != "11111111-2222-4333-8444-555555555555" ||
		instance.Name != "cs-11111111222243338444" ||
		instance.RuntimeState != RuntimeStateRunning ||
		instance.RepoTag != "default" {
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

func TestBootstrapCredentialFilesUseFixedPathsAndModes(t *testing.T) {
	t.Parallel()

	files := bootstrapCredentialFiles(CredentialRequest{
		GiteaToken: "gitea-token",
	})
	got := make([]bootstrapCredentialFile, len(files))
	copy(got, files)
	want := []bootstrapCredentialFile{
		{
			path: runtimeCredentialDir,
			mode: runtimeCredentialDirMode,
			kind: "directory",
		},
		{
			path: runtimeGitCredentialDir,
			mode: runtimeCredentialDirMode,
			kind: "directory",
		},
		{
			path:    runtimeGiteaTokenFilePath,
			content: "gitea-token",
			mode:    runtimeCredentialFileMode,
			kind:    "file",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("credential files = %#v", got)
	}
}

func TestDecodeEndpointManifestFile(t *testing.T) {
	t.Parallel()

	content := `{"version":1,"endpoints":[{"endpoint_id":"web","label":"Web","upstream_scheme":"http","upstream_port":3000,"public":true}]}`
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var manifest runtimeEndpointManifestFile
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode endpoint manifest: %v", err)
	}
	if manifest.Version != 1 ||
		len(manifest.Endpoints) != 1 ||
		manifest.Endpoints[0].EndpointID != "web" ||
		manifest.Endpoints[0].UpstreamPort != 3000 ||
		!manifest.Endpoints[0].Public {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestCredentialFileIDUsesScriptIdentity(t *testing.T) {
	t.Parallel()

	if got := credentialFileID(1000, 0); got != 1000 {
		t.Fatalf("script identity = %d", got)
	}
	if got := credentialFileID(0, 7); got != 7 {
		t.Fatalf("fallback identity = %d", got)
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
		{name: "virtual machine", value: "virtual-machine", want: api.InstanceTypeVM},
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

func TestNormalizeIncusTemplatesRequiresCompleteTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template IncusTemplateConfig
		want     string
	}{
		{name: "image", template: IncusTemplateConfig{InstanceType: "container", CommunicationInterface: "eth0", CPU: 1, MemoryLimit: "1GiB", RootDiskSize: "10GiB", Profiles: []string{"default"}}, want: "image"},
		{name: "communication nic", template: IncusTemplateConfig{Image: "images:debian/12", InstanceType: "container", CPU: 1, MemoryLimit: "1GiB", RootDiskSize: "10GiB", Profiles: []string{"default"}}, want: "communication_nic"},
		{name: "cpu", template: IncusTemplateConfig{Image: "images:debian/12", InstanceType: "container", CommunicationInterface: "eth0", MemoryLimit: "1GiB", RootDiskSize: "10GiB", Profiles: []string{"default"}}, want: "cpu"},
		{name: "memory", template: IncusTemplateConfig{Image: "images:debian/12", InstanceType: "container", CommunicationInterface: "eth0", CPU: 1, RootDiskSize: "10GiB", Profiles: []string{"default"}}, want: "memory"},
		{name: "root disk", template: IncusTemplateConfig{Image: "images:debian/12", InstanceType: "container", CommunicationInterface: "eth0", CPU: 1, MemoryLimit: "1GiB", Profiles: []string{"default"}}, want: "root_disk_size"},
		{name: "profiles", template: IncusTemplateConfig{Image: "images:debian/12", InstanceType: "container", CommunicationInterface: "eth0", CPU: 1, MemoryLimit: "1GiB", RootDiskSize: "10GiB"}, want: "profiles"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := normalizeIncusTemplates(IncusConfig{
				Templates: map[string]IncusTemplateConfig{"default": tt.template},
			})
			if err == nil {
				t.Fatalf("expected template validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("template validation error = %v", err)
			}
		})
	}
}

func TestIncusCreateRequestUsesTemplateResources(t *testing.T) {
	t.Parallel()

	request := incusCreateRequest(InstanceSpec{
		CodespaceUUID: "11111111-1111-4111-8111-111111111111",
		Name:          "cs-test",
		RepoTag:       "default",
	}, incusTemplate{
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
		incusConfigManagerID:     "7",
		incusConfigCodespaceUUID: "11111111-1111-4111-8111-111111111111",
		incusConfigTag:           "default",
	})

	if request.Type != api.InstanceTypeContainer {
		t.Fatalf("instance type = %q", request.Type)
	}
	if request.Config["limits.cpu"] != "2" || request.Config["limits.memory"] != "1GiB" {
		t.Fatalf("instance config = %#v", request.Config)
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

func TestIncusStartupAdmissionUsesProjectInstanceQuota(t *testing.T) {
	t.Parallel()

	admission, err := incusTemplateStartupAdmission(&api.ProjectState{
		Resources: map[string]api.ProjectStateResource{
			"instances": {Limit: 1, Usage: 1},
		},
	}, incusTemplate{instanceType: api.InstanceTypeContainer, memoryLimit: "1GiB"})
	if err != nil {
		t.Fatalf("startup admission: %v", err)
	}
	if admission.CreateAvailable {
		t.Fatalf("create should be unavailable when project instance quota is full")
	}
	if !admission.ResumeAvailable {
		t.Fatalf("resume should remain available when project instance quota is full")
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
			admission, err := incusTemplateStartupAdmission(&api.ProjectState{Resources: tt.resources}, incusTemplate{
				instanceType: tt.instance,
				memoryLimit:  tt.memoryLimit,
			})
			if err != nil {
				t.Fatalf("startup admission: %v", err)
			}
			if admission.CreateAvailable != tt.wantCreate {
				t.Fatalf("create available = %v, want %v", admission.CreateAvailable, tt.wantCreate)
			}
			if !admission.ResumeAvailable {
				t.Fatalf("resume should remain available")
			}
		})
	}
}

func TestIncusStartupAdmissionRequiresEveryTemplateToFit(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{
		templates: map[string]incusTemplate{
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
	if admission.CreateAvailable {
		t.Fatalf("create should be unavailable when one declared template exceeds project quota")
	}
	if !admission.ResumeAvailable {
		t.Fatalf("resume should remain available")
	}
}

func TestWorkspaceSFTPHandlersMapRequestsUnderWorkspace(t *testing.T) {
	t.Parallel()

	instanceClient, closeInstance := newTestSFTPClient(t, sftp.InMemHandler())
	defer closeInstance()
	if err := instanceClient.Mkdir("/workspaces"); err != nil {
		t.Fatalf("mkdir /workspaces: %v", err)
	}
	if err := instanceClient.Mkdir("/workspaces/repo"); err != nil {
		t.Fatalf("mkdir /workspaces/repo: %v", err)
	}
	workspaceClient, closeWorkspace := newTestSFTPClient(t, workspaceSFTPHandlers(instanceClient, "/workspaces/repo"))
	defer closeWorkspace()

	if err := workspaceClient.Mkdir("/dir"); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	file, err := workspaceClient.Create("/dir/file.txt")
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

	escaped, err := workspaceClient.Create("/../outside.txt")
	if err != nil {
		t.Fatalf("create cleaned workspace file: %v", err)
	}
	_ = escaped.Close()
	if _, err := instanceClient.Stat("/outside.txt"); err == nil {
		t.Fatalf("path escaped workspace root")
	}
	if _, err := instanceClient.Stat("/workspaces/repo/outside.txt"); err != nil {
		t.Fatalf("cleaned path was not written under workspace: %v", err)
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

func TestParseSharedEnvKeepsLastValueAndIgnoresPredefined(t *testing.T) {
	t.Parallel()

	values, err := parseSharedEnv("A=1\nCODESPACE_UUID=ignored\nA=2\nPRIVATE=value=with=equals\n", map[string]struct{}{
		"CODESPACE_UUID": {},
	})
	if err != nil {
		t.Fatalf("parse shared env: %v", err)
	}
	want := map[string]string{
		"A":       "2",
		"PRIVATE": "value=with=equals",
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("shared env = %#v", values)
	}
}

func TestParseSharedEnvTrimsTrailingNULPadding(t *testing.T) {
	t.Parallel()

	values, err := parseSharedEnv("A=1\nB=2\n\x00\x00", nil)
	if err != nil {
		t.Fatalf("parse shared env: %v", err)
	}
	want := map[string]string{
		"A": "1",
		"B": "2",
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("shared env = %#v", values)
	}
}

func TestParseSharedEnvRejectsInvalidContent(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		"NO_EQUALS\n",
		"BAD-NAME=value\n",
		"A=value\x00\n",
		"A=value\n\x00\n",
	} {
		if _, err := parseSharedEnv(content, nil); err == nil {
			t.Fatalf("expected error for %q", content)
		}
	}
}

func TestValidateScriptResultRequiresDoneAtExpectedStage(t *testing.T) {
	t.Parallel()

	if err := validateScriptResult(`{"outcome":"done","stage":"prepare-workspace"}`, "prepare-workspace"); err != nil {
		t.Fatalf("validate result: %v", err)
	}
	recoverable := []string{
		`{"outcome":"recoverable_failed","stage":"prepare-workspace"}`,
		`{"outcome":"unknown","stage":"prepare-workspace"}`,
		`{"outcome":"done","stage":"start-environment"}`,
		`{"outcome":"done","stage":"prepare-workspace","extra":true}`,
	}
	for _, content := range recoverable {
		if err := validateScriptResult(content, "prepare-workspace"); err == nil {
			t.Fatalf("expected result error for %s", content)
		} else if !IsRecoverableScriptFailure(err) {
			t.Fatalf("expected recoverable result error for %s, got %v", content, err)
		}
	}
	err := validateScriptResult(`{"outcome":"unrecoverable_failed","stage":"prepare-workspace"}`, "prepare-workspace")
	var failure *ScriptFailureError
	if !errors.As(err, &failure) || failure.Kind != ScriptFailureUnrecoverable {
		t.Fatalf("unrecoverable result error = %#v", err)
	}
}

func TestLoadScriptSetReadsCompleteCustomSuite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	initPath := filepath.Join(dir, "init.sh")
	startPath := filepath.Join(dir, "start.sh")
	resumePath := filepath.Join(dir, "resume.sh")
	for _, path := range []string{initPath, startPath, resumePath} {
		if err := os.WriteFile(path, []byte("echo ok\n"), 0o600); err != nil {
			t.Fatalf("write script: %v", err)
		}
	}
	scripts, err := LoadScripts(ScriptConfig{Init: initPath, Start: startPath, Resume: resumePath})
	if err != nil {
		t.Fatalf("load scripts: %v", err)
	}
	if scripts.Init.Content != "echo ok\n" ||
		scripts.Start.Content != "echo ok\n" ||
		scripts.Resume.Content != "echo ok\n" ||
		scripts.Init.SHA256 == "" ||
		scripts.Start.SHA256 == "" ||
		scripts.Resume.SHA256 == "" {
		t.Fatalf("scripts = %#v", scripts)
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

func TestInstanceStateCommunicationHostUsesGlobalAddress(t *testing.T) {
	t.Parallel()

	state := &api.InstanceState{
		Network: map[string]api.InstanceStateNetwork{
			"eth0": {
				Addresses: []api.InstanceStateNetworkAddress{
					{Address: "fe80::1", Scope: "link"},
					{Address: "10.0.0.12", Scope: "global"},
				},
			},
			"eth1": {
				Addresses: []api.InstanceStateNetworkAddress{
					{Address: "10.0.1.12", Scope: "global"},
				},
			},
		},
	}

	if host := instanceStateCommunicationHost(state, "eth0"); host != "10.0.0.12" {
		t.Fatalf("eth0 host = %q", host)
	}
	if host := instanceStateCommunicationHost(state, "eth2"); host != "" {
		t.Fatalf("missing interface host = %q", host)
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
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if err := validateIncusServer(test.server, test.project); err == nil {
				t.Fatalf("expected validation error")
			}
		})
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
