// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/units"
)

const defaultCodespaceRoot = "/codespace"
const defaultCommunicationInterface = "eth0"
const defaultIncusImage = "images:debian/12"
const defaultIncusInstanceType = "container"
const defaultBootstrapShell = "/bin/sh"
const defaultBootstrapHomeDir = "/root"
const defaultBootstrapUserName = "codespace"
const defaultVMAgentWaitTimeout = 2 * time.Minute
const maxIncusExecCapturedOutput = 1024 * 1024

const (
	runtimeCredentialDir        = "/var/lib/gitea-codespace"
	runtimeCredentialSeedDir    = "/var/lib/gitea-codespace/seed"
	runtimeSeedGiteaToken       = "/var/lib/gitea-codespace/seed/gitea-token"
	runtimeSeedGitSSHPrivateKey = "/var/lib/gitea-codespace/seed/id_ed25519"
	runtimeSeedGitSSHPublicKey  = "/var/lib/gitea-codespace/seed/id_ed25519.pub"
	runtimeSeedGitSSHKnownHosts = "/var/lib/gitea-codespace/seed/known_hosts"
	runtimeGitCredentialDir     = "/var/lib/gitea-codespace/git"
	runtimeManifestDir          = "/var/lib/gitea-codespace/runtime"
	runtimeGiteaTokenFilePath   = "/var/lib/gitea-codespace/gitea-token"
	runtimeEndpointManifest     = "/var/lib/gitea-codespace/runtime/endpoints.json"
	runtimeRepositoryConfig     = "/var/lib/gitea-codespace/runtime/repository-codespace.yaml"
	runtimeGitSSHPrivateKey     = "/var/lib/gitea-codespace/git/id_ed25519"
	runtimeGitSSHPublicKey      = "/var/lib/gitea-codespace/git/id_ed25519.pub"
	runtimeGitSSHKnownHosts     = "/var/lib/gitea-codespace/git/known_hosts"
	runtimeSharedEnvFilePath    = "/var/lib/gitea-codespace/env"
	runtimeScriptResultDir      = "/var/lib/gitea-codespace/results"
	runtimeScriptParentDir      = "/usr/local/libexec"
	runtimeScriptDir            = "/usr/local/libexec/gitea-codespace"
	runtimeRootDirMode          = 0o755
	runtimePrivateDirMode       = 0o700
	runtimeCredentialFileMode   = 0o600
	runtimeCredentialWriteMode  = "overwrite"
)

const (
	incusConfigManagerID      = "user.gitea.manager_id"
	incusConfigCodespaceUUID  = "user.gitea.codespace_uuid"
	incusConfigSchemaVersion  = "user.gitea.schema_version"
	incusConfigEnvironmentTag = "user.gitea.environment_tag"
	incusConfigE2ERunID       = "user.gitea.e2e_run_id"
)

type bootstrapCredentialFile struct {
	path    string
	content string
	mode    int
}

type runtimeEndpointManifestFile struct {
	Version   int                          `json:"version"`
	Endpoints []RuntimeEndpointDeclaration `json:"endpoints"`
}

// IncusConfig configures one Incus-backed provisioner.
type IncusConfig struct {
	ManagerID           int64
	Project             string
	ProjectManage       bool
	Remote              string
	UnixSocket          string
	StoragePool         string
	NetworkName         string
	NetworkManage       bool
	RuntimeEnvironments map[string]IncusEnvironmentConfig
	ExtraConfig         map[string]string
	CodespaceRoot       string
	Bootstrap           BootstrapConfig
	Scripts             ScriptConfig
}

// IncusEnvironmentConfig stores one Incus environment selected by repository tag.
type IncusEnvironmentConfig struct {
	Image                  string
	InstanceType           string
	CPU                    int32
	MemoryLimit            string
	RootDiskSize           string
	Profiles               []string
	CommunicationInterface string
	SourceType             string
	SourceRemote           string
	SourceProject          string
	SourceName             string
}

// IncusProvisioner provisions codespace as Incus instances.
type IncusProvisioner struct {
	client        incus.InstanceServer
	managerID     string
	project       string
	environments  map[string]incusEnvironment
	extraConfig   map[string]string
	codespaceRoot string
	bootstrap     BootstrapConfig
	scripts       ScriptSnapshot
	mu            sync.Mutex
	cpuSamples    map[string]incusCPUSample
}

type incusEnvironment struct {
	image                  string
	instanceType           api.InstanceType
	cpu                    int32
	memoryLimit            string
	rootDiskSize           string
	profiles               []string
	communicationInterface string
	sourceType             string
	sourceRemote           string
	sourceProject          string
	sourceName             string
}

type incusCPUSample struct {
	usage        int64
	observedUnix int64
}

// NewIncus creates one Incus-backed provisioner.
func NewIncus(config IncusConfig) (*IncusProvisioner, error) {
	if config.ManagerID <= 0 {
		return nil, fmt.Errorf("manager_id is required")
	}
	client, err := connectIncusBase(config)
	if err != nil {
		return nil, fmt.Errorf("connect incus: %w", err)
	}
	if err := ensureIncusProject(client, config); err != nil {
		return nil, err
	}
	client = withProject(client, config.Project)
	server, _, err := client.GetServer()
	if err != nil {
		return nil, fmt.Errorf("get incus server: %w", err)
	}
	if err := validateIncusServer(server, config.Project); err != nil {
		return nil, err
	}
	project := strings.TrimSpace(config.Project)
	if project == "" {
		project = strings.TrimSpace(server.Environment.Project)
	}
	if project == "" {
		project = api.ProjectDefaultName
	}
	if err := ensureIncusManagedResources(client, config); err != nil {
		return nil, err
	}

	codespaceRoot := config.CodespaceRoot
	if codespaceRoot == "" {
		codespaceRoot = defaultCodespaceRoot
	}
	scripts, err := LoadScripts(config.Scripts)
	if err != nil {
		return nil, err
	}
	environments, err := normalizeIncusEnvironments(config)
	if err != nil {
		return nil, err
	}

	bootstrap := normalizedBootstrapConfig(config.Bootstrap)

	return &IncusProvisioner{
		client:        client,
		managerID:     fmt.Sprintf("%d", config.ManagerID),
		project:       project,
		environments:  environments,
		extraConfig:   copyStringMap(config.ExtraConfig),
		codespaceRoot: codespaceRoot,
		bootstrap:     bootstrap,
		scripts:       scripts,
		cpuSamples:    make(map[string]incusCPUSample),
	}, nil
}

// CheckStartupAdmission reports whether Incus project quotas allow new startup work.
func (p *IncusProvisioner) CheckStartupAdmission(ctx context.Context) (StartupAdmission, error) {
	if err := ctx.Err(); err != nil {
		return StartupAdmission{}, err
	}
	state, err := p.client.GetProjectState(p.project)
	if err != nil {
		return StartupAdmission{}, fmt.Errorf("get incus project state %s: %w", p.project, err)
	}
	return p.incusStartupAdmission(state)
}

func (p *IncusProvisioner) incusStartupAdmission(state *api.ProjectState) (StartupAdmission, error) {
	admission := StartupAdmission{ResumeAvailable: true, CreateAvailable: true}
	for tag, environment := range p.environments {
		environmentAdmission, err := incusEnvironmentStartupAdmission(state, environment)
		if err != nil {
			return StartupAdmission{}, fmt.Errorf("check environment %s startup admission: %w", tag, err)
		}
		if !environmentAdmission.CreateAvailable {
			admission.CreateAvailable = false
		}
	}
	return admission, nil
}

func incusEnvironmentStartupAdmission(state *api.ProjectState, environment incusEnvironment) (StartupAdmission, error) {
	admission := StartupAdmission{ResumeAvailable: true, CreateAvailable: true}
	if state == nil || len(state.Resources) == 0 {
		return admission, nil
	}
	if !projectResourceAvailable(state.Resources, "instances", 1) {
		admission.CreateAvailable = false
	}
	switch environment.instanceType {
	case api.InstanceTypeContainer:
		if !projectResourceAvailable(state.Resources, "containers", 1) {
			admission.CreateAvailable = false
		}
	case api.InstanceTypeVM:
		if !projectResourceAvailable(state.Resources, "virtual-machines", 1) {
			admission.CreateAvailable = false
		}
	}
	if memory, ok := state.Resources["memory"]; ok && memory.Limit >= 0 {
		memoryLimit := strings.TrimSpace(environment.memoryLimit)
		if memoryLimit == "" {
			admission.CreateAvailable = false
			return admission, nil
		}
		required, err := units.ParseByteSizeString(memoryLimit)
		if err != nil {
			return StartupAdmission{}, fmt.Errorf("parse incus environment memory %q: %w", memoryLimit, err)
		}
		if memory.Usage+required > memory.Limit {
			admission.CreateAvailable = false
		}
	}
	if disk, ok := state.Resources["disk"]; ok && disk.Limit >= 0 {
		rootDiskSize := strings.TrimSpace(environment.rootDiskSize)
		if rootDiskSize == "" {
			admission.CreateAvailable = false
			return admission, nil
		}
		required, err := units.ParseByteSizeString(rootDiskSize)
		if err != nil {
			return StartupAdmission{}, fmt.Errorf("parse incus environment root disk size %q: %w", rootDiskSize, err)
		}
		if disk.Usage+required > disk.Limit {
			admission.CreateAvailable = false
		}
	}
	return admission, nil
}

func projectResourceAvailable(resources map[string]api.ProjectStateResource, name string, required int64) bool {
	resource, ok := resources[name]
	if !ok || resource.Limit < 0 {
		return true
	}
	return resource.Usage+required <= resource.Limit
}

func normalizeIncusEnvironments(config IncusConfig) (map[string]incusEnvironment, error) {
	if len(config.RuntimeEnvironments) == 0 {
		return nil, fmt.Errorf("incus environments are required")
	}
	normalized := make(map[string]incusEnvironment, len(config.RuntimeEnvironments))
	for tag, environment := range config.RuntimeEnvironments {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, fmt.Errorf("incus environment tag is empty")
		}
		sourceType := strings.TrimSpace(environment.SourceType)
		if sourceType == "" {
			sourceType = "image"
		}
		image := strings.TrimSpace(environment.Image)
		sourceName := strings.TrimSpace(environment.SourceName)
		switch sourceType {
		case "image":
			if image == "" {
				return nil, fmt.Errorf("incus environment %s image is required", tag)
			}
		case "instance":
			if sourceName == "" {
				return nil, fmt.Errorf("incus environment %s source instance name is required", tag)
			}
			sourceRemote := strings.TrimSpace(environment.SourceRemote)
			if sourceRemote != "" && sourceRemote != "local" {
				return nil, fmt.Errorf("incus environment %s source remote %q is not supported; configure the manager to connect to that Incus server and clone by project/name", tag, sourceRemote)
			}
		default:
			return nil, fmt.Errorf("incus environment %s source type must be image or instance", tag)
		}
		instanceType, err := normalizeIncusInstanceType(environment.InstanceType)
		if err != nil {
			return nil, fmt.Errorf("incus environment %s: %w", tag, err)
		}
		communicationInterface := strings.TrimSpace(environment.CommunicationInterface)
		if communicationInterface == "" {
			return nil, fmt.Errorf("incus environment %s communication_interface is required", tag)
		}
		if environment.CPU < 1 {
			return nil, fmt.Errorf("incus environment %s cpu must be positive", tag)
		}
		if strings.TrimSpace(environment.MemoryLimit) == "" {
			return nil, fmt.Errorf("incus environment %s memory is required", tag)
		}
		if strings.TrimSpace(environment.RootDiskSize) == "" {
			return nil, fmt.Errorf("incus environment %s resources.root_disk is required", tag)
		}
		if len(environment.Profiles) == 0 {
			return nil, fmt.Errorf("incus environment %s profiles are required", tag)
		}
		normalized[tag] = incusEnvironment{
			image:                  image,
			instanceType:           instanceType,
			cpu:                    environment.CPU,
			memoryLimit:            strings.TrimSpace(environment.MemoryLimit),
			rootDiskSize:           strings.TrimSpace(environment.RootDiskSize),
			profiles:               normalizedIncusProfiles(environment.Profiles),
			communicationInterface: communicationInterface,
			sourceType:             sourceType,
			sourceRemote:           strings.TrimSpace(environment.SourceRemote),
			sourceProject:          strings.TrimSpace(environment.SourceProject),
			sourceName:             sourceName,
		}
	}
	return normalized, nil
}

func normalizeIncusInstanceType(value string) (api.InstanceType, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = defaultIncusInstanceType
	}
	switch value {
	case "container", "lxc":
		return api.InstanceTypeContainer, nil
	case "virtual-machine", "vm":
		return api.InstanceTypeVM, nil
	default:
		return "", fmt.Errorf("incus instance type must be lxc/container or vm/virtual-machine")
	}
}

func normalizedBootstrapConfig(config BootstrapConfig) BootstrapConfig {
	config.Shell = strings.TrimSpace(config.Shell)
	if config.Shell == "" {
		config.Shell = defaultBootstrapShell
	}
	config.HomeDir = strings.TrimSpace(config.HomeDir)
	if config.HomeDir == "" {
		config.HomeDir = defaultBootstrapHomeDir
	}
	config.UserName = strings.TrimSpace(config.UserName)
	if config.UserName == "" {
		config.UserName = defaultBootstrapUserName
	}
	return config
}

// CreateOrStart creates or starts one instance.
func (p *IncusProvisioner) CreateOrStart(ctx context.Context, spec InstanceSpec) (*Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	instanceName := spec.Name
	if instanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}

	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		if !isNotFoundError(err) {
			return nil, fmt.Errorf("get instance %s: %w", instanceName, err)
		}
		environment, err := p.environmentForTag(spec.EnvironmentTag)
		if err != nil {
			return nil, err
		}
		if err := p.createInstance(ctx, spec, environment); err != nil {
			return nil, fmt.Errorf("create instance %s: %w", instanceName, err)
		}
		instance, _, err = p.client.GetInstance(instanceName)
		if err != nil {
			return nil, fmt.Errorf("reload instance %s: %w", instanceName, err)
		}
	}

	return p.startExistingInstance(ctx, spec, *instance)
}

// StartExisting starts one existing instance.
func (p *IncusProvisioner) StartExisting(ctx context.Context, spec InstanceSpec) (*Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	instanceName := spec.Name
	if instanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		if isNotFoundError(err) {
			return nil, fmt.Errorf("instance %s does not exist", instanceName)
		}
		return nil, fmt.Errorf("get instance %s: %w", instanceName, err)
	}
	return p.startExistingInstance(ctx, spec, *instance)
}

// InitializeSystem runs init.sh and returns the credential file identity.
func (p *IncusProvisioner) InitializeSystem(ctx context.Context, instanceName string, request BootstrapRequest) (SystemIdentity, error) {
	if err := ctx.Err(); err != nil {
		return SystemIdentity{}, err
	}
	if instanceName == "" {
		return SystemIdentity{}, fmt.Errorf("instance name is empty")
	}
	scripts := p.scriptsForRequest(request)
	env, err := p.runLifecycleScript(ctx, instanceName, "init.sh", scripts.Init.Content, "initialize-system", request)
	if err != nil {
		return SystemIdentity{}, err
	}
	uid, err := parseUint32Env(env, "CODESPACE_CREDENTIAL_UID")
	if err != nil {
		return SystemIdentity{}, err
	}
	gid, err := parseUint32Env(env, "CODESPACE_CREDENTIAL_GID")
	if err != nil {
		return SystemIdentity{}, err
	}
	if uid == 0 || gid == 0 {
		return SystemIdentity{}, fmt.Errorf("credential uid and gid must be non-root")
	}
	return SystemIdentity{UID: uid, GID: gid, SharedEnv: copyStringMap(env)}, nil
}

// PrepareWorkspace runs start.sh/resume.sh prepare and returns the workspace path.
func (p *IncusProvisioner) PrepareWorkspace(ctx context.Context, instanceName string, request BootstrapRequest) (WorkspaceStatus, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceStatus{}, err
	}
	if instanceName == "" {
		return WorkspaceStatus{}, fmt.Errorf("instance name is empty")
	}
	scripts := p.scriptsForRequest(request)
	scriptName, script := p.scriptForOperation(scripts, request.Operation)
	env, err := p.runLifecycleScript(ctx, instanceName, scriptName, script, "prepare-workspace", request)
	if err != nil {
		return WorkspaceStatus{}, err
	}
	workdir := strings.TrimSpace(env["CODESPACE_WORKSPACE_DIR"])
	if !filepath.IsAbs(workdir) {
		return WorkspaceStatus{}, fmt.Errorf("CODESPACE_WORKSPACE_DIR must be absolute")
	}
	return WorkspaceStatus{Workdir: workdir, SharedEnv: copyStringMap(env)}, nil
}

// ActivateRuntime runs start.sh/resume.sh activate and returns shared environment.
func (p *IncusProvisioner) ActivateRuntime(ctx context.Context, instanceName string, request BootstrapRequest) (RuntimeAccess, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeAccess{}, err
	}
	if instanceName == "" {
		return RuntimeAccess{}, fmt.Errorf("instance name is empty")
	}
	scripts := p.scriptsForRequest(request)
	scriptName, script := p.scriptForOperation(scripts, request.Operation)
	env, err := p.runLifecycleScript(ctx, instanceName, scriptName, script, "start-environment", request)
	if err != nil {
		return RuntimeAccess{}, err
	}
	return RuntimeAccess{
		SharedEnv: copyStringMap(env),
	}, nil
}

func (p *IncusProvisioner) scriptsForRequest(request BootstrapRequest) ScriptSnapshot {
	if scriptSnapshotComplete(request.Scripts) {
		return request.Scripts
	}
	return p.scripts
}

func (p *IncusProvisioner) scriptForOperation(scripts ScriptSnapshot, operation ScriptOperation) (string, string) {
	if operation == ScriptOperationResume {
		return "resume.sh", scripts.Resume.Content
	}
	return "start.sh", scripts.Start.Content
}

// CheckCredentials reads the current runtime credential files from the instance.
func (p *IncusProvisioner) CheckCredentials(ctx context.Context, instanceName string) (CredentialStatus, error) {
	if err := ctx.Err(); err != nil {
		return CredentialStatus{}, err
	}
	if instanceName == "" {
		return CredentialStatus{}, fmt.Errorf("instance name is empty")
	}
	giteaToken, giteaPresent, err := p.readCredentialFile(ctx, instanceName, runtimeGiteaTokenFilePath)
	if err != nil {
		return CredentialStatus{}, err
	}
	gitSSHPrivateKey, _, err := p.readCredentialFile(ctx, instanceName, runtimeGitSSHPrivateKey)
	if err != nil {
		return CredentialStatus{}, err
	}
	gitSSHPublicKey, _, err := p.readCredentialFile(ctx, instanceName, runtimeGitSSHPublicKey)
	if err != nil {
		return CredentialStatus{}, err
	}
	return CredentialStatus{
		GiteaTokenPresent: giteaPresent && strings.TrimSpace(giteaToken) != "",
		GitSSHPrivateKey:  []byte(gitSSHPrivateKey),
		GitSSHPublicKey:   []byte(gitSSHPublicKey),
	}, nil
}

// CheckWorkspaceGit verifies the workspace origin has local credentials for the current protocol.
func (p *IncusProvisioner) CheckWorkspaceGit(ctx context.Context, instanceName string, workdir string) (WorkspaceGitStatus, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceGitStatus{}, err
	}
	if instanceName == "" {
		return WorkspaceGitStatus{}, fmt.Errorf("instance name is empty")
	}
	workdir = strings.TrimSpace(workdir)
	if !filepath.IsAbs(workdir) {
		return WorkspaceGitStatus{}, fmt.Errorf("workdir must be absolute")
	}
	output, err := p.execScriptOutput(ctx, instanceName, `
set -eu
workdir="${CODESPACE_WORKSPACE_DIR}"
[ -d "$workdir/.git" ] || exit 60
origin="$(git -C "$workdir" remote get-url origin)"
helper="$(git -C "$workdir" config --get credential.helper || true)"
global_helper="$(git config --global --get credential.helper || true)"
ssh_command="$(git -C "$workdir" config --get core.sshCommand || true)"
printf 'ORIGIN=%s\n' "$origin"
printf 'HELPER=%s\n' "$helper"
printf 'GLOBAL_HELPER=%s\n' "$global_helper"
printf 'SSH_COMMAND=%s\n' "$ssh_command"
`, map[string]string{"CODESPACE_WORKSPACE_DIR": workdir}, "/")
	if err != nil {
		return WorkspaceGitStatus{}, err
	}
	values, err := parseSharedEnv(output, nil)
	if err != nil {
		return WorkspaceGitStatus{}, fmt.Errorf("parse git status: %w", err)
	}
	origin := strings.TrimSpace(values["ORIGIN"])
	helper := values["HELPER"]
	globalHelper := values["GLOBAL_HELPER"]
	sshCommand := values["SSH_COMMAND"]
	return WorkspaceGitStatus{
		OriginURL:            origin,
		CredentialConfigured: workspaceGitCredentialConfigured(origin, helper, globalHelper, sshCommand),
	}, nil
}

// CheckWorkspaceAccess verifies that Gateway-backed workspace entry can reach the runtime workdir.
func (p *IncusProvisioner) CheckWorkspaceAccess(ctx context.Context, instanceName string, workdir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(instanceName) == "" {
		return fmt.Errorf("instance name is empty")
	}
	workdir = strings.TrimSpace(workdir)
	if !filepath.IsAbs(workdir) {
		return fmt.Errorf("workdir must be absolute")
	}
	return p.execScript(ctx, instanceName, `
set -eu
workdir="${CODESPACE_WORKSPACE_DIR}"
[ -d "$workdir" ] || exit 70
[ -w "$workdir" ] || exit 71
probe="$workdir/.gitea-codespace-health-$$"
: > "$probe"
rm -f "$probe"
`, map[string]string{"CODESPACE_WORKSPACE_DIR": workdir}, "/")
}

func workspaceGitCredentialConfigured(origin, helper, globalHelper, sshCommand string) bool {
	origin = strings.TrimSpace(origin)
	switch {
	case strings.HasPrefix(origin, "http://") || strings.HasPrefix(origin, "https://"):
		return strings.Contains(helper, "gitea-codespace-git-credential") ||
			strings.Contains(globalHelper, "gitea-codespace-git-credential")
	case origin != "":
		return strings.Contains(sshCommand, "/var/lib/gitea-codespace/git/id_ed25519") &&
			strings.Contains(sshCommand, "/var/lib/gitea-codespace/git/known_hosts") &&
			strings.Contains(sshCommand, "StrictHostKeyChecking=yes")
	default:
		return false
	}
}

func (p *IncusProvisioner) readCredentialFile(ctx context.Context, instanceName, path string) (string, bool, error) {
	if err := p.waitInstanceFileAPI(ctx, instanceName); err != nil {
		return "", false, err
	}
	content, _, err := p.client.GetInstanceFile(instanceName, path)
	if err != nil {
		if isNotFoundError(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	defer content.Close()
	data, err := io.ReadAll(io.LimitReader(content, 4096))
	if err != nil {
		return "", false, fmt.Errorf("read %s content: %w", path, err)
	}
	return string(data), true, nil
}

// SeedRuntimeCredentials writes root-owned credential seed files.
func (p *IncusProvisioner) SeedRuntimeCredentials(ctx context.Context, instanceName string, request RuntimeCredentialSeedRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(instanceName) == "" {
		return fmt.Errorf("instance name is empty")
	}
	files, err := runtimeCredentialSeedFiles(request)
	if err != nil {
		return err
	}
	for _, directory := range []struct {
		path string
		mode int
	}{
		{path: runtimeCredentialDir, mode: runtimeRootDirMode},
		{path: runtimeCredentialSeedDir, mode: runtimePrivateDirMode},
	} {
		if err := p.writeRuntimeFile(ctx, instanceName, directory.path, "", directory.mode, "directory"); err != nil {
			return err
		}
	}
	for _, file := range files {
		if err := p.writeRuntimeOwnedFile(ctx, instanceName, file.path, file.content, 0, 0, file.mode); err != nil {
			return fmt.Errorf("write %s: %w", file.path, err)
		}
	}
	return nil
}

func runtimeCredentialSeedFiles(request RuntimeCredentialSeedRequest) ([]bootstrapCredentialFile, error) {
	if strings.TrimSpace(request.CodespaceUUID) == "" {
		return nil, fmt.Errorf("codespace uuid is empty")
	}
	if strings.TrimSpace(request.GiteaToken) == "" {
		return nil, fmt.Errorf("gitea token is empty")
	}
	if len(request.GitSSHPrivateKey) == 0 {
		return nil, fmt.Errorf("git ssh private key is empty")
	}
	if len(request.GitSSHPublicKey) == 0 {
		return nil, fmt.Errorf("git ssh public key is empty")
	}
	content := strings.TrimSpace(strings.Join(request.GitSSHKnownHosts, "\n"))
	if content == "" {
		return nil, fmt.Errorf("git ssh known hosts are empty")
	}
	content += "\n"
	return []bootstrapCredentialFile{
		{path: runtimeSeedGiteaToken, content: request.GiteaToken, mode: runtimeCredentialFileMode},
		{path: runtimeSeedGitSSHPrivateKey, content: string(request.GitSSHPrivateKey), mode: runtimeCredentialFileMode},
		{path: runtimeSeedGitSSHPublicKey, content: string(request.GitSSHPublicKey), mode: 0o644},
		{path: runtimeSeedGitSSHKnownHosts, content: content, mode: runtimeCredentialFileMode},
	}, nil
}

// ReadEndpointManifest reads the runtime endpoint declaration file.
func (p *IncusProvisioner) ReadEndpointManifest(ctx context.Context, instanceName string) ([]RuntimeEndpointDeclaration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(instanceName) == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	content, exists, err := p.readRuntimeFile(ctx, instanceName, runtimeEndpointManifest)
	if err != nil {
		return nil, err
	}
	if !exists || strings.TrimSpace(content) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var manifest runtimeEndpointManifestFile
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode endpoint manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode endpoint manifest: trailing data")
	}
	if manifest.Version != 1 {
		return nil, fmt.Errorf("endpoint manifest version %d is not supported", manifest.Version)
	}
	return append([]RuntimeEndpointDeclaration(nil), manifest.Endpoints...), nil
}

// Stop stops one instance if it exists.
func (p *IncusProvisioner) Stop(ctx context.Context, instanceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" {
		return nil
	}

	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("get instance %s: %w", instanceName, err)
	}
	if strings.EqualFold(instance.Status, "Stopped") {
		return nil
	}

	operation, err := p.client.UpdateInstanceState(instanceName, api.InstanceStatePut{
		Action:  "stop",
		Force:   true,
		Timeout: -1,
	}, "")
	if err != nil {
		return fmt.Errorf("stop instance %s: %w", instanceName, err)
	}
	if err := operation.WaitContext(ctx); err != nil {
		cancelIncusOperationOnContextError(ctx, operation)
		return fmt.Errorf("wait stop instance %s: %w", instanceName, err)
	}
	return nil
}

// Delete deletes one instance if it exists.
func (p *IncusProvisioner) Delete(ctx context.Context, instanceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" {
		return nil
	}

	if err := p.Stop(ctx, instanceName); err != nil {
		return err
	}

	operation, err := p.client.DeleteInstance(instanceName)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("delete instance %s: %w", instanceName, err)
	}
	if err := operation.WaitContext(ctx); err != nil {
		cancelIncusOperationOnContextError(ctx, operation)
		return fmt.Errorf("wait delete instance %s: %w", instanceName, err)
	}
	return nil
}

func (p *IncusProvisioner) createInstance(ctx context.Context, spec InstanceSpec, environment incusEnvironment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	instanceConfig := map[string]string{
		incusConfigManagerID:      p.managerID,
		incusConfigCodespaceUUID:  spec.CodespaceUUID,
		incusConfigSchemaVersion:  "1",
		incusConfigEnvironmentTag: spec.EnvironmentTag,
	}
	for key, value := range p.extraConfig {
		key = strings.TrimSpace(key)
		if key != "" {
			instanceConfig[key] = value
		}
	}
	rootDiskName := ""
	rootDiskDevice := map[string]string(nil)
	if environment.rootDiskSize != "" {
		var err error
		rootDiskName, rootDiskDevice, err = p.rootDiskDevice(environment.profiles)
		if err != nil {
			return err
		}
	}
	request := incusCreateRequest(spec, environment, rootDiskName, rootDiskDevice, instanceConfig)

	operation, err := p.client.CreateInstance(request)
	if err != nil {
		return fmt.Errorf("create instance request: %w", err)
	}
	if err := operation.WaitContext(ctx); err != nil {
		cancelIncusOperationOnContextError(ctx, operation)
		return fmt.Errorf("wait create instance: %w", err)
	}
	return nil
}

func incusCreateRequest(spec InstanceSpec, environment incusEnvironment, rootDiskName string, rootDiskDevice map[string]string, instanceConfig map[string]string) api.InstancesPost {
	if environment.memoryLimit != "" {
		instanceConfig["limits.memory"] = environment.memoryLimit
	}
	if environment.cpu > 0 {
		instanceConfig["limits.cpu"] = fmt.Sprintf("%d", environment.cpu)
	}
	instanceDevices := map[string]map[string]string{}
	if environment.rootDiskSize != "" {
		device := copyStringMap(rootDiskDevice)
		device["type"] = "disk"
		device["path"] = "/"
		device["size"] = environment.rootDiskSize
		instanceDevices[rootDiskName] = device
	}
	return api.InstancesPost{
		Name: spec.Name,
		Type: environment.instanceType,
		InstancePut: api.InstancePut{
			Config:   instanceConfig,
			Devices:  instanceDevices,
			Profiles: append([]string(nil), environment.profiles...),
		},
		Source: incusInstanceSource(environment),
	}
}

func incusInstanceSource(environment incusEnvironment) api.InstanceSource {
	if environment.sourceType == "instance" {
		return api.InstanceSource{
			Type:         "copy",
			Source:       environment.sourceName,
			Project:      environment.sourceProject,
			InstanceOnly: true,
		}
	}
	return api.InstanceSource{
		Type:        "image",
		Alias:       incusImageAlias(environment.image),
		Fingerprint: incusImageFingerprint(environment.image),
		Server:      imageServerForAlias(environment.image),
		Protocol:    imageProtocolForAlias(environment.image),
	}
}

func (p *IncusProvisioner) rootDiskDevice(profiles []string) (string, map[string]string, error) {
	var rootDiskName string
	var rootDiskDevice map[string]string
	for _, profileName := range profiles {
		profile, _, err := p.client.GetProfile(profileName)
		if err != nil {
			return "", nil, fmt.Errorf("get profile %s: %w", profileName, err)
		}
		for name, device := range profile.Devices {
			if device["type"] != "disk" || device["path"] != "/" {
				continue
			}
			if rootDiskName != "" {
				return "", nil, fmt.Errorf("profiles %s define multiple root disk devices", strings.Join(profiles, ","))
			}
			rootDiskName = name
			rootDiskDevice = copyStringMap(device)
		}
	}
	if rootDiskName == "" {
		return "", nil, fmt.Errorf("profiles %s do not define a root disk", strings.Join(profiles, ","))
	}
	return rootDiskName, rootDiskDevice, nil
}

func normalizedIncusProfiles(profiles []string) []string {
	normalized := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		profile = strings.TrimSpace(profile)
		if profile == "" {
			continue
		}
		normalized = append(normalized, profile)
	}
	if len(normalized) == 0 {
		return []string{"default"}
	}
	return normalized
}

// ListInstances returns all Codespace instances owned by this provisioner.
func (p *IncusProvisioner) ListInstances(ctx context.Context) ([]*Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	instances, err := p.client.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	result := make([]*Instance, 0, len(instances))
	for _, instance := range instances {
		owned, ok := p.instanceFromAPI(instance)
		if !ok {
			continue
		}
		result = append(result, owned)
	}
	return result, nil
}

func (p *IncusProvisioner) instanceFromAPI(instance api.Instance) (*Instance, bool) {
	if strings.TrimSpace(instance.Config[incusConfigManagerID]) != p.managerID {
		return nil, false
	}
	codespaceUUID := strings.TrimSpace(instance.Config[incusConfigCodespaceUUID])
	if codespaceUUID == "" {
		return nil, false
	}
	return &Instance{
		CodespaceUUID:  codespaceUUID,
		Name:           instance.Name,
		RuntimeState:   incusRuntimeState(instance.Status),
		EnvironmentTag: instance.Config[incusConfigEnvironmentTag],
	}, true
}

func (p *IncusProvisioner) startInstance(ctx context.Context, instanceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		return fmt.Errorf("get instance %s: %w", instanceName, err)
	}
	if strings.EqualFold(instance.Status, "Running") {
		return nil
	}

	operation, err := p.client.UpdateInstanceState(instanceName, api.InstanceStatePut{
		Action:  "start",
		Timeout: -1,
	}, "")
	if err != nil {
		return fmt.Errorf("start instance request: %w", err)
	}
	if err := operation.WaitContext(ctx); err != nil {
		cancelIncusOperationOnContextError(ctx, operation)
		return fmt.Errorf("wait start instance: %w", err)
	}
	return nil
}

func (p *IncusProvisioner) startExistingInstance(ctx context.Context, spec InstanceSpec, instance api.Instance) (*Instance, error) {
	tag := strings.TrimSpace(instance.Config[incusConfigEnvironmentTag])
	if tag == "" {
		tag = spec.EnvironmentTag
	}
	environment, err := p.environmentForTag(tag)
	if err != nil {
		return nil, err
	}
	if err := p.startInstance(ctx, instance.Name); err != nil {
		return nil, fmt.Errorf("start instance %s: %w", instance.Name, err)
	}
	host, err := p.instanceCommunicationHost(ctx, instance.Name, environment.communicationInterface)
	if err != nil {
		return nil, err
	}
	return &Instance{
		CodespaceUUID:     spec.CodespaceUUID,
		Name:              instance.Name,
		RuntimeState:      RuntimeStateRunning,
		Workdir:           filepath.Join(p.codespaceRoot, repoDirName(spec.RepoFullName)),
		RepoFullName:      spec.RepoFullName,
		EnvironmentTag:    spec.EnvironmentTag,
		CommunicationHost: host,
	}, nil
}

func (p *IncusProvisioner) environmentForTag(tag string) (incusEnvironment, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = "default"
	}
	environment, ok := p.environments[tag]
	if !ok {
		return incusEnvironment{}, fmt.Errorf("incus environment %s is not configured", tag)
	}
	return environment, nil
}

func (p *IncusProvisioner) instanceCommunicationHost(ctx context.Context, instanceName string, communicationInterface string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 60; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		state, _, err := p.client.GetInstanceState(instanceName)
		if err != nil {
			lastErr = fmt.Errorf("get instance state %s: %w", instanceName, err)
		} else if host := instanceStateCommunicationHost(state, communicationInterface); host != "" {
			return host, nil
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("instance %s has no global address on communication interface %s", instanceName, communicationInterface)
}

// RuntimeResourceUsage samples CPU, memory and disk usage from Incus.
func (p *IncusProvisioner) RuntimeResourceUsage(ctx context.Context, instanceName string) (RuntimeResourceUsage, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeResourceUsage{}, err
	}
	state, _, err := p.client.GetInstanceState(instanceName)
	if err != nil {
		return RuntimeResourceUsage{}, fmt.Errorf("get instance state %s: %w", instanceName, err)
	}
	now := time.Now().Unix()
	usage := instanceStateResourceUsage(state, now)
	usage.CPUUsedMillicores, usage.CPUObserved = p.cpuUsageMillicores(instanceName, state, now)
	return usage, nil
}

func (p *IncusProvisioner) cpuUsageMillicores(instanceName string, state *api.InstanceState, observedUnix int64) (int64, bool) {
	if state == nil || observedUnix <= 0 {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	current := incusCPUSample{usage: state.CPU.Usage, observedUnix: observedUnix}
	previous, ok := p.cpuSamples[instanceName]
	p.cpuSamples[instanceName] = current
	if !ok || previous.observedUnix <= 0 || current.observedUnix <= previous.observedUnix || current.usage < previous.usage {
		return 0, false
	}
	elapsedNanos := (current.observedUnix - previous.observedUnix) * int64(time.Second)
	if elapsedNanos <= 0 {
		return 0, false
	}
	return (current.usage - previous.usage) * 1000 / elapsedNanos, true
}

func instanceStateResourceUsage(state *api.InstanceState, observedUnix int64) RuntimeResourceUsage {
	if state == nil {
		return RuntimeResourceUsage{ObservedUnix: observedUnix}
	}
	diskUsed, diskLimit := instanceStateDiskUsage(state.Disk)
	return RuntimeResourceUsage{
		CPULimitMillicores: state.CPU.AllocatedTime / int64(time.Millisecond),
		MemoryUsedBytes:    state.Memory.Usage,
		MemoryLimitBytes:   state.Memory.Total,
		DiskUsedBytes:      diskUsed,
		DiskLimitBytes:     diskLimit,
		ObservedUnix:       observedUnix,
	}
}

func instanceStateDiskUsage(disks map[string]api.InstanceStateDisk) (int64, int64) {
	var used, limit int64
	for _, disk := range disks {
		if disk.Usage > 0 {
			used += disk.Usage
		}
		if disk.Total > 0 {
			limit += disk.Total
		}
	}
	return used, limit
}

func instanceStateCommunicationHost(state *api.InstanceState, interfaceName string) string {
	if state == nil {
		return ""
	}
	if strings.TrimSpace(interfaceName) != "" {
		return networkCommunicationHost(state.Network[interfaceName])
	}
	for _, network := range state.Network {
		if host := networkCommunicationHost(network); host != "" {
			return host
		}
	}
	return ""
}

func networkCommunicationHost(network api.InstanceStateNetwork) string {
	for _, address := range network.Addresses {
		if strings.EqualFold(strings.TrimSpace(address.Scope), "link") ||
			strings.EqualFold(strings.TrimSpace(address.Scope), "local") {
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(address.Address))
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		return ip.String()
	}
	return ""
}

func incusRuntimeState(status string) RuntimeState {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return RuntimeStateRunning
	case "stopped":
		return RuntimeStateStopped
	default:
		return RuntimeStateCreating
	}
}

func connectIncusBase(config IncusConfig) (incus.InstanceServer, error) {
	if config.Remote != "" {
		client, err := incus.ConnectIncus(config.Remote, nil)
		if err != nil {
			return nil, fmt.Errorf("connect remote %s: %w", config.Remote, err)
		}
		return client, nil
	}

	client, err := incus.ConnectIncusUnix(config.UnixSocket, nil)
	if err != nil {
		return nil, fmt.Errorf("connect unix socket %q: %w", config.UnixSocket, err)
	}
	return client, nil
}

func connectIncus(config IncusConfig) (incus.InstanceServer, error) {
	client, err := connectIncusBase(config)
	if err != nil {
		return nil, err
	}
	return withProject(client, config.Project), nil
}

func withProject(client incus.InstanceServer, project string) incus.InstanceServer {
	if project == "" {
		return client
	}
	return client.UseProject(project)
}

func ensureIncusProject(client incus.InstanceServer, config IncusConfig) error {
	if !config.ProjectManage {
		return nil
	}
	projectName := strings.TrimSpace(config.Project)
	if projectName == "" || projectName == api.ProjectDefaultName {
		return nil
	}
	project, _, err := client.GetProject(projectName)
	if err != nil {
		if !isNotFoundError(err) {
			return fmt.Errorf("get incus project %s: %w", projectName, err)
		}
		err = client.CreateProject(api.ProjectsPost{
			Name: projectName,
			ProjectPut: api.ProjectPut{
				Description: "Gitea Codespace runtimes",
				Config: map[string]string{
					"features.profiles":        "true",
					"features.networks":        "true",
					"features.storage.volumes": "true",
				},
			},
		})
		if err != nil {
			return fmt.Errorf("create incus project %s: %w", projectName, err)
		}
		project, _, err = client.GetProject(projectName)
		if err != nil {
			return fmt.Errorf("reload incus project %s: %w", projectName, err)
		}
	}
	for _, feature := range []string{"features.profiles", "features.networks", "features.storage.volumes"} {
		if !projectFeatureEnabled(project.Config, feature) {
			return fmt.Errorf("incus project %s must enable %s before it is used by codespace manager", projectName, feature)
		}
	}
	return nil
}

func projectFeatureEnabled(config map[string]string, name string) bool {
	value := strings.TrimSpace(config[name])
	return strings.EqualFold(value, "true") || value == "1"
}

func ensureIncusManagedResources(client incus.InstanceServer, config IncusConfig) error {
	if !config.ProjectManage {
		return nil
	}
	if config.NetworkManage {
		if err := ensureIncusNetwork(client, config.NetworkName); err != nil {
			return err
		}
	}
	return ensureIncusDefaultProfile(client, config)
}

func ensureIncusNetwork(client incus.InstanceServer, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("incus managed network name is required")
	}
	if network, _, err := client.GetNetwork(name); err == nil {
		if network.Type != "bridge" || !network.Managed {
			return fmt.Errorf("incus network %s must be a managed bridge network", name)
		}
		return nil
	} else if !isNotFoundError(err) {
		return fmt.Errorf("get incus network %s: %w", name, err)
	}
	if err := client.CreateNetwork(api.NetworksPost{
		Name: name,
		Type: "bridge",
		NetworkPut: api.NetworkPut{
			Description: "Gitea Codespace managed network",
			Config: map[string]string{
				"ipv4.address": "auto",
				"ipv4.dhcp":    "true",
				"ipv4.nat":     "true",
				"ipv6.address": "none",
			},
		},
	}); err != nil {
		return fmt.Errorf("create incus network %s: %w", name, err)
	}
	return nil
}

func ensureIncusDefaultProfile(client incus.InstanceServer, config IncusConfig) error {
	storagePool := strings.TrimSpace(config.StoragePool)
	if storagePool == "" {
		return fmt.Errorf("incus storage pool is required when project management is enabled")
	}
	if _, _, err := client.GetStoragePool(storagePool); err != nil {
		return fmt.Errorf("get incus storage pool %s: %w", storagePool, err)
	}
	networkName := strings.TrimSpace(config.NetworkName)
	devices := map[string]map[string]string{
		"root": {
			"type": "disk",
			"path": "/",
			"pool": storagePool,
		},
	}
	if networkName != "" {
		devices["eth0"] = map[string]string{
			"type":    "nic",
			"network": networkName,
		}
	}
	profilePut := api.ProfilePut{
		Description: "Gitea Codespace default profile",
		Devices:     devices,
		Config:      map[string]string{},
	}
	profile, etag, err := client.GetProfile("default")
	if err != nil {
		if !isNotFoundError(err) {
			return fmt.Errorf("get incus profile default: %w", err)
		}
		return client.CreateProfile(api.ProfilesPost{
			Name:       "default",
			ProfilePut: profilePut,
		})
	}
	if !profileHasManagedDevices(profile, storagePool, networkName) {
		operationErr := client.UpdateProfile("default", profilePut, etag)
		if operationErr != nil {
			return fmt.Errorf("update incus profile default: %w", operationErr)
		}
	}
	return nil
}

func profileHasManagedDevices(profile *api.Profile, storagePool, networkName string) bool {
	if profile == nil {
		return false
	}
	root := profile.Devices["root"]
	if root["type"] != "disk" || root["path"] != "/" || root["pool"] != storagePool {
		return false
	}
	if strings.TrimSpace(networkName) == "" {
		return true
	}
	nic := profile.Devices["eth0"]
	return nic["type"] == "nic" && nic["network"] == networkName
}

func validateIncusServer(server *api.Server, project string) error {
	if server == nil {
		return fmt.Errorf("incus server response is empty")
	}
	if !strings.EqualFold(strings.TrimSpace(server.Environment.Server), "incus") {
		return fmt.Errorf("incus server implementation is %q", server.Environment.Server)
	}
	if !strings.EqualFold(strings.TrimSpace(server.Auth), "trusted") {
		return fmt.Errorf("incus client is not trusted")
	}
	if server.Public {
		return fmt.Errorf("incus server is public-only")
	}
	if server.Environment.ServerClustered {
		return fmt.Errorf("incus clustered mode is not supported")
	}
	project = strings.TrimSpace(project)
	if project != "" && strings.TrimSpace(server.Environment.Project) != project {
		return fmt.Errorf("incus project %q is not active", project)
	}
	return nil
}

func incusImageAlias(value string) string {
	value = trimRemoteAlias(value)
	if isLikelyIncusFingerprint(value) {
		return ""
	}
	return value
}

func incusImageFingerprint(value string) string {
	value = trimRemoteAlias(value)
	if isLikelyIncusFingerprint(value) {
		return value
	}
	return ""
}

func trimRemoteAlias(value string) string {
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return strings.TrimSpace(value)
}

func imageServerForAlias(value string) string {
	if strings.HasPrefix(value, "images:") {
		return "https://images.linuxcontainers.org"
	}
	return ""
}

func imageProtocolForAlias(value string) string {
	if imageServerForAlias(value) != "" {
		return "simplestreams"
	}
	return ""
}

func isLikelyIncusFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func buildGitURLPrefixes(repoURL string, username string, token string) (string, string, error) {
	if username == "" || token == "" {
		return "", "", nil
	}

	parsedURL, err := url.Parse(repoURL)
	if err != nil {
		return "", "", fmt.Errorf("parse repo url %q: %w", repoURL, err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return "", "", nil
	}

	baseURL := &url.URL{
		Scheme: parsedURL.Scheme,
		Host:   parsedURL.Host,
		Path:   "/",
	}
	authURL := *baseURL
	authURL.User = url.UserPassword(username, token)
	return authURL.String(), baseURL.String(), nil
}

func (p *IncusProvisioner) execScript(
	ctx context.Context,
	instanceName string,
	script string,
	environment map[string]string,
	workdir string,
) error {
	return p.execCommand(ctx, instanceName, []string{p.bootstrap.Shell, "-lc", script}, environment, workdir)
}

func (p *IncusProvisioner) execScriptOutput(
	ctx context.Context,
	instanceName string,
	script string,
	environment map[string]string,
	workdir string,
) (string, error) {
	return p.execCommandOutput(ctx, instanceName, []string{p.bootstrap.Shell, "-lc", script}, environment, workdir)
}

func (p *IncusProvisioner) execCommand(
	ctx context.Context,
	instanceName string,
	command []string,
	environment map[string]string,
	workdir string,
) error {
	_, err := p.execInstanceCommand(ctx, instanceName, command, environment, workdir, false, nil)
	return err
}

func (p *IncusProvisioner) execCommandWithLogSink(
	ctx context.Context,
	instanceName string,
	command []string,
	environment map[string]string,
	workdir string,
	sink LifecycleLogSink,
) error {
	_, err := p.execInstanceCommand(ctx, instanceName, command, environment, workdir, false, sink)
	return err
}

func (p *IncusProvisioner) execCommandOutput(
	ctx context.Context,
	instanceName string,
	command []string,
	environment map[string]string,
	workdir string,
) (string, error) {
	return p.execInstanceCommand(ctx, instanceName, command, environment, workdir, true, nil)
}

func (p *IncusProvisioner) execInstanceCommand(
	ctx context.Context,
	instanceName string,
	command []string,
	environment map[string]string,
	workdir string,
	requireCompleteStdout bool,
	sink LifecycleLogSink,
) (string, error) {
	if err := p.waitInstanceFileAPI(ctx, instanceName); err != nil {
		return "", err
	}
	stdout := newBoundedOutputBuffer(maxIncusExecCapturedOutput)
	stderr := newBoundedOutputBuffer(maxIncusExecCapturedOutput)
	stdoutWriter := io.Writer(stdout)
	stderrWriter := io.Writer(stderr)
	var stdoutLog, stderrLog *lifecycleLogWriter
	if sink != nil {
		stdoutLog = newLifecycleLogWriter(ctx, sink, "stdout")
		stderrLog = newLifecycleLogWriter(ctx, sink, "stderr")
		stdoutWriter = io.MultiWriter(stdout, stdoutLog)
		stderrWriter = io.MultiWriter(stderr, stderrLog)
		defer stdoutLog.Flush()
		defer stderrLog.Flush()
	}

	operation, err := p.client.ExecInstance(instanceName, api.InstanceExecPost{
		Command:     command,
		WaitForWS:   true,
		Environment: environment,
		Cwd:         workdir,
		User:        p.bootstrap.User,
		Group:       p.bootstrap.Group,
	}, &incus.InstanceExecArgs{
		Stdout: stdoutWriter,
		Stderr: stderrWriter,
	})
	if err != nil {
		return "", fmt.Errorf("exec instance command: %w", err)
	}
	if err := operation.WaitContext(ctx); err != nil {
		cancelIncusOperationOnContextError(ctx, operation)
		return "", fmt.Errorf(
			"wait instance command: %w (stdout=%q stderr=%q)",
			err,
			stdout.String(),
			stderr.String(),
		)
	}
	if status, ok := incusOperationExitStatus(operation); ok && status != 0 {
		return "", fmt.Errorf(
			"instance command exited with status %d (stdout=%q stderr=%q)",
			status,
			stdout.String(),
			stderr.String(),
		)
	}
	if requireCompleteStdout && stdout.Truncated() {
		return "", fmt.Errorf("instance command stdout exceeded %d bytes", maxIncusExecCapturedOutput)
	}
	return stdout.String(), nil
}

func cancelIncusOperationOnContextError(ctx context.Context, operation incus.Operation) {
	if ctx == nil || operation == nil || ctx.Err() == nil {
		return
	}
	_ = operation.Cancel()
}

type boundedOutputBuffer struct {
	limit     int
	data      []byte
	truncated int
}

func newBoundedOutputBuffer(limit int) *boundedOutputBuffer {
	return &boundedOutputBuffer{limit: limit}
}

func (b *boundedOutputBuffer) Write(payload []byte) (int, error) {
	if b == nil {
		return len(payload), nil
	}
	if b.limit <= 0 {
		b.truncated += len(payload)
		return len(payload), nil
	}
	if len(payload) >= b.limit {
		b.truncated += len(b.data) + len(payload) - b.limit
		b.data = append(b.data[:0], payload[len(payload)-b.limit:]...)
		return len(payload), nil
	}
	overflow := len(b.data) + len(payload) - b.limit
	if overflow > 0 {
		b.truncated += overflow
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, payload...)
	return len(payload), nil
}

func (b *boundedOutputBuffer) String() string {
	if b == nil {
		return ""
	}
	if !b.Truncated() {
		return string(b.data)
	}
	return fmt.Sprintf("<truncated %d bytes>\n%s", b.truncated, string(b.data))
}

func (b *boundedOutputBuffer) Truncated() bool {
	return b != nil && b.truncated > 0
}

type lifecycleLogWriter struct {
	ctx    context.Context
	sink   LifecycleLogSink
	stream string
	mu     sync.Mutex
	buf    []byte
}

func newLifecycleLogWriter(ctx context.Context, sink LifecycleLogSink, stream string) *lifecycleLogWriter {
	return &lifecycleLogWriter{ctx: ctx, sink: sink, stream: stream}
}

func (w *lifecycleLogWriter) Write(payload []byte) (int, error) {
	if w == nil || w.sink == nil {
		return len(payload), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, payload...)
	for {
		index := bytes.IndexByte(w.buf, '\n')
		if index < 0 {
			break
		}
		w.writeLineLocked(w.buf[:index])
		w.buf = w.buf[index+1:]
	}
	return len(payload), nil
}

func (w *lifecycleLogWriter) Flush() {
	if w == nil || w.sink == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return
	}
	w.writeLineLocked(w.buf)
	w.buf = nil
}

func (w *lifecycleLogWriter) writeLineLocked(line []byte) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	message := string(line)
	if message == "" {
		return
	}
	if w.stream != "" {
		message = w.stream + ": " + message
	}
	_ = w.sink.WriteLifecycleLog(w.ctx, message)
}

func (p *IncusProvisioner) waitInstanceFileAPI(ctx context.Context, instanceName string) error {
	deadline := time.Now().Add(defaultVMAgentWaitTimeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.probeVMAgent(instanceName); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait vm agent %s: %w", instanceName, lastErr)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *IncusProvisioner) probeVMAgent(instanceName string) error {
	return p.client.CreateInstanceFile(instanceName, runtimeCredentialDir, incus.InstanceFileArgs{
		UID:       int64(p.bootstrap.User),
		GID:       int64(p.bootstrap.Group),
		Mode:      runtimeRootDirMode,
		Type:      "directory",
		WriteMode: runtimeCredentialWriteMode,
	})
}

func (p *IncusProvisioner) writeRuntimeOwnedFile(ctx context.Context, instanceName, path, content string, uid, gid int64, mode int) error {
	if err := p.waitInstanceFileAPI(ctx, instanceName); err != nil {
		return err
	}
	if err := p.client.DeleteInstanceFile(instanceName, path); err != nil && !isNotFoundError(err) {
		return fmt.Errorf("delete previous %s: %w", path, err)
	}
	return p.createInstanceFile(ctx, instanceName, path, incus.InstanceFileArgs{
		Content:   strings.NewReader(content),
		UID:       uid,
		GID:       gid,
		Mode:      mode,
		Type:      "file",
		WriteMode: runtimeCredentialWriteMode,
	})
}

func (p *IncusProvisioner) createInstanceFile(ctx context.Context, instanceName, path string, args incus.InstanceFileArgs) error {
	deadline := time.Now().Add(defaultVMAgentWaitTimeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.client.CreateInstanceFile(instanceName, path, args); err != nil {
			lastErr = err
			if path != runtimeCredentialDir && !isIncusVMAgentUnavailable(err) {
				return err
			}
		} else {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait instance file api %s: %w", instanceName, lastErr)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func isIncusVMAgentUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "VM agent isn't currently running") ||
		strings.Contains(message, "VM agent isn't currently connected") ||
		strings.Contains(message, "VM agent")
}

func isNotFoundError(err error) bool {
	var apiStatus api.StatusError
	return errors.As(err, &apiStatus) && apiStatus.Status() == 404
}
