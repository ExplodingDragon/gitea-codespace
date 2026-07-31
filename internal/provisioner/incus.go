// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/units"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/devcontainerruntime"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

const defaultCodespaceRoot = "/codespace"
const defaultIncusImage = "images:debian/12"
const defaultIncusInstanceType = "container"
const defaultBootstrapShell = "/bin/bash"
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
	runtimeStateDir             = "/var/lib/gitea-codespace/state"
	runtimeGiteaTokenFilePath   = "/var/lib/gitea-codespace/gitea-token"
	runtimeEndpointManifest     = runtimeendpoint.EndpointManifestPath
	runtimeGitSSHPrivateKey     = "/var/lib/gitea-codespace/git/id_ed25519"
	runtimeGitSSHPublicKey      = "/var/lib/gitea-codespace/git/id_ed25519.pub"
	runtimeGitSSHKnownHosts     = "/var/lib/gitea-codespace/git/known_hosts"
	runtimeBootstrapOutputFile  = "/var/lib/gitea-codespace/state/bootstrap.env"
	bootstrapResultDir          = "/var/lib/gitea-codespace/state/results"
	runtimeExecutableDir        = "/usr/local/libexec"
	bootstrapScriptDir          = "/usr/local/libexec/gitea-codespace"
	runtimeSecretDir            = "/run/gitea-codespace"
	runtimeSecretFile           = "/run/gitea-codespace/secrets.json"
	runtimeExecutable           = "/usr/local/libexec/gitea-codespace-runtime-host"
	runtimeEnvironmentFile      = "/var/lib/gitea-codespace/state/environment.json"
	runtimeRequestFile          = "/var/lib/gitea-codespace/state/request.json"
	runtimeResultFile           = "/var/lib/gitea-codespace/state/result.json"
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
	RuntimeExecutable   string
	CodeServerVersion   string
	BuildCacheRegistry  string
	RegistryMirrors     map[string]string
}

// IncusEnvironmentConfig stores one deployment-defined Incus runtime environment.
type IncusEnvironmentConfig struct {
	Image         string
	InstanceType  string
	CPU           int32
	MemoryLimit   string
	RootDiskSize  string
	Profiles      []string
	SourceType    string
	SourceProject string
	SourceName    string
}

// IncusProvisioner provisions codespace as Incus instances.
type IncusProvisioner struct {
	client             incus.InstanceServer
	managerID          string
	project            string
	networkName        string
	environments       map[string]incusEnvironment
	extraConfig        map[string]string
	codespaceRoot      string
	bootstrap          BootstrapConfig
	runtimeBinary      string
	codeServerVersion  string
	buildCacheRegistry string
	registryMirrors    map[string]string
	mu                 sync.Mutex
	cpuSamples         map[string]incusCPUSample
}

type incusEnvironment struct {
	image         string
	instanceType  api.InstanceType
	cpu           int32
	memoryLimit   string
	rootDiskSize  string
	profiles      []string
	sourceType    string
	sourceProject string
	sourceName    string
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
	networkName := strings.TrimSpace(config.NetworkName)
	if networkName == "" {
		return nil, fmt.Errorf("incus network name is required")
	}
	baseClient, err := connectIncusBase(config)
	if err != nil {
		return nil, fmt.Errorf("connect incus: %w", err)
	}
	if err := ensureIncusProject(baseClient, config); err != nil {
		return nil, err
	}
	client := withProject(baseClient, config.Project)
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
	if err := ensureIncusManagedResources(baseClient, client, config); err != nil {
		return nil, err
	}

	codespaceRoot := config.CodespaceRoot
	if codespaceRoot == "" {
		codespaceRoot = defaultCodespaceRoot
	}
	environments, err := normalizeIncusEnvironments(config)
	if err != nil {
		return nil, err
	}

	bootstrap := normalizedBootstrapConfig(config.Bootstrap)

	return &IncusProvisioner{
		client:             client,
		managerID:          fmt.Sprintf("%d", config.ManagerID),
		project:            project,
		networkName:        networkName,
		environments:       environments,
		extraConfig:        copyStringMap(config.ExtraConfig),
		codespaceRoot:      codespaceRoot,
		bootstrap:          bootstrap,
		runtimeBinary:      strings.TrimSpace(config.RuntimeExecutable),
		codeServerVersion:  strings.TrimSpace(config.CodeServerVersion),
		buildCacheRegistry: strings.TrimRight(strings.TrimSpace(config.BuildCacheRegistry), "/"),
		registryMirrors:    copyStringMap(config.RegistryMirrors),
		cpuSamples:         make(map[string]incusCPUSample),
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
	admission := StartupAdmission{ResumeAvailable: true, CreateTags: make([]string, 0, len(p.environments))}
	for tag, environment := range p.environments {
		available, err := incusEnvironmentCreateAvailable(state, environment)
		if err != nil {
			return StartupAdmission{}, fmt.Errorf("check environment %s startup admission: %w", tag, err)
		}
		if available {
			admission.CreateTags = append(admission.CreateTags, tag)
		}
	}
	slices.Sort(admission.CreateTags)
	return admission, nil
}

func incusEnvironmentCreateAvailable(state *api.ProjectState, environment incusEnvironment) (bool, error) {
	if state == nil || len(state.Resources) == 0 {
		return true, nil
	}
	if !projectResourceAvailable(state.Resources, "instances", 1) {
		return false, nil
	}
	switch environment.instanceType {
	case api.InstanceTypeContainer:
		if !projectResourceAvailable(state.Resources, "containers", 1) {
			return false, nil
		}
	case api.InstanceTypeVM:
		if !projectResourceAvailable(state.Resources, "virtual-machines", 1) {
			return false, nil
		}
	}
	if memory, ok := state.Resources["memory"]; ok && memory.Limit >= 0 {
		memoryLimit := strings.TrimSpace(environment.memoryLimit)
		if memoryLimit == "" {
			return false, nil
		}
		required, err := units.ParseByteSizeString(memoryLimit)
		if err != nil {
			return false, fmt.Errorf("parse incus environment memory %q: %w", memoryLimit, err)
		}
		if memory.Usage+required > memory.Limit {
			return false, nil
		}
	}
	if disk, ok := state.Resources["disk"]; ok && disk.Limit >= 0 {
		rootDiskSize := strings.TrimSpace(environment.rootDiskSize)
		if rootDiskSize == "" {
			return false, nil
		}
		required, err := units.ParseByteSizeString(rootDiskSize)
		if err != nil {
			return false, fmt.Errorf("parse incus environment root disk size %q: %w", rootDiskSize, err)
		}
		if disk.Usage+required > disk.Limit {
			return false, nil
		}
	}
	return true, nil
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
		default:
			return nil, fmt.Errorf("incus environment %s source type must be image or instance", tag)
		}
		instanceType, err := normalizeIncusInstanceType(environment.InstanceType)
		if err != nil {
			return nil, fmt.Errorf("incus environment %s: %w", tag, err)
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
			image:         image,
			instanceType:  instanceType,
			cpu:           environment.CPU,
			memoryLimit:   strings.TrimSpace(environment.MemoryLimit),
			rootDiskSize:  strings.TrimSpace(environment.RootDiskSize),
			profiles:      normalizedIncusProfiles(environment.Profiles),
			sourceType:    sourceType,
			sourceProject: strings.TrimSpace(environment.SourceProject),
			sourceName:    sourceName,
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

// BootstrapSystem runs the fixed bootstrap and returns the runtime identity and workspace.
func (p *IncusProvisioner) BootstrapSystem(ctx context.Context, instanceName string, request LifecycleRequest) (SystemIdentity, error) {
	if err := ctx.Err(); err != nil {
		return SystemIdentity{}, err
	}
	if instanceName == "" {
		return SystemIdentity{}, fmt.Errorf("instance name is empty")
	}
	env, err := p.runBootstrap(ctx, instanceName, request)
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
	workspace := strings.TrimSpace(env["CODESPACE_WORKSPACE_DIR"])
	if !filepath.IsAbs(workspace) {
		return SystemIdentity{}, fmt.Errorf("bootstrap workspace path must be absolute")
	}
	home, err := p.execScriptOutput(ctx, instanceName, `getent passwd "$CODESPACE_UID" | cut -d: -f6`, map[string]string{"CODESPACE_UID": fmt.Sprintf("%d", uid)}, "/")
	if err != nil || !filepath.IsAbs(strings.TrimSpace(home)) {
		return SystemIdentity{}, fmt.Errorf("resolve runtime user home: %w", err)
	}
	return SystemIdentity{UID: uid, GID: gid, Workspace: workspace, Home: strings.TrimSpace(home)}, nil
}

// StartEnvironment creates or resumes the native Dev Container environment.
func (p *IncusProvisioner) StartEnvironment(ctx context.Context, instanceName string, request LifecycleRequest) (LifecycleResult, error) {
	action := "create"
	if request.Operation == LifecycleOperationResume {
		action = "resume"
	}
	return p.applyRuntime(ctx, instanceName, request, action)
}

// StopEnvironment stops every container that belongs to the saved environment.
func (p *IncusProvisioner) StopEnvironment(ctx context.Context, instanceName string, request LifecycleRequest) (LifecycleResult, error) {
	return p.applyRuntime(ctx, instanceName, request, "stop")
}

func (p *IncusProvisioner) applyRuntime(ctx context.Context, instanceName string, request LifecycleRequest, action string) (LifecycleResult, error) {
	if err := ctx.Err(); err != nil {
		return LifecycleResult{}, err
	}
	if strings.TrimSpace(instanceName) == "" {
		return LifecycleResult{}, fmt.Errorf("instance name is empty")
	}
	if err := p.installRuntimeExecutable(ctx, instanceName); err != nil {
		return LifecycleResult{}, err
	}
	secrets := map[string]string{}
	if content, exists, err := p.readRuntimeFile(ctx, instanceName, runtimeSecretFile); err != nil {
		return LifecycleResult{}, err
	} else if exists {
		decoder := json.NewDecoder(strings.NewReader(content))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&secrets); err != nil {
			return LifecycleResult{}, fmt.Errorf("decode runtime secrets: %w", err)
		}
	}
	source := devcontainer.Source{
		Path:          request.DevContainer.Path,
		ContentSHA256: request.DevContainer.ContentSHA256,
	}
	if request.DevContainer.Source == "platform_default" {
		source = devcontainer.Source{DefaultImage: request.DevContainer.DefaultImage}
	}
	runtimeRequest := devcontainerruntime.Request{
		Version:          devcontainerruntime.FormatVersion,
		Action:           action,
		CodespaceUUID:    request.CodespaceUUID,
		OperationVersion: request.OperationVersion,
		Workspace:        request.Workdir,
		Source:           source,
		HostUser: devcontainer.HostUser{
			Name: request.RuntimeUserName,
		},
		GitUserName:  request.UserName,
		GitUserEmail: request.GitUserEmail,
		LocalEnvironment: map[string]string{
			"GITEA_SERVER_URL":     request.ServerURL,
			"GITEA_REPOSITORY":     request.RepoFullName,
			"GITEA_CODESPACE_UUID": request.CodespaceUUID,
		},
		Secrets:           secrets,
		CodeServerVersion: p.codeServerVersion,
		Environment:       request.Environment,
	}
	if request.Operation == LifecycleOperationCreate {
		cacheIdentity := strings.Join([]string{
			"1", request.RepoFullName, request.CommitSHA, request.DevContainer.ContentSHA256,
			p.codeServerVersion, runtime.GOOS, runtime.GOARCH,
		}, "\x00")
		cacheDigest := sha256.Sum256([]byte(cacheIdentity))
		runtimeRequest.Cache = devcontainer.CacheOptions{
			BuildRegistry: p.buildCacheRegistry,
			Mirrors:       copyStringMap(p.registryMirrors),
			BuildScope:    fmt.Sprintf("%x", cacheDigest[:]),
		}
		uid, gid, home, err := p.runtimeUserIdentity(ctx, instanceName, request.RuntimeUserName)
		if err != nil {
			return LifecycleResult{}, err
		}
		runtimeRequest.HostUser.UID = uid
		runtimeRequest.HostUser.GID = gid
		runtimeRequest.HostUser.Home = home
	} else if request.Environment != nil {
		// Resume and stop use the target fixed at create time; the outer UID/GID are not consumed by the Docker engine.
		runtimeRequest.HostUser.Name = request.Environment.RemoteUser
	}
	encoded, err := json.Marshal(runtimeRequest)
	if err != nil {
		return LifecycleResult{}, err
	}
	if err := p.writeRuntimeFile(ctx, instanceName, runtimeRequestFile, string(encoded), 0o600, "file"); err != nil {
		return LifecycleResult{}, err
	}
	if err := p.writeRuntimeFile(ctx, instanceName, runtimeResultFile, "", 0o600, "file"); err != nil {
		return LifecycleResult{}, err
	}
	p.writeLifecycleLog(ctx, request.LogSink, action+" Dev Container environment started")
	execErr := p.execCommandWithLogSink(ctx, instanceName, []string{runtimeExecutable, "runtime", "apply", "--request", runtimeRequestFile, "--result", runtimeResultFile}, map[string]string{
		"DOCKER_BUILDKIT":   "1",
		"BUILDKIT_PROGRESS": "plain",
	}, "/", request.LogSink)
	requestCleanupErr := p.deleteRuntimeControlFile(instanceName, runtimeRequestFile)
	content, exists, readErr := p.readRuntimeFile(ctx, instanceName, runtimeResultFile)
	resultCleanupErr := p.deleteRuntimeControlFile(instanceName, runtimeResultFile)
	if readErr != nil || !exists {
		return LifecycleResult{}, fmt.Errorf("read native runtime result: %w", errors.Join(execErr, readErr, requestCleanupErr, resultCleanupErr))
	}
	if cleanupErr := errors.Join(requestCleanupErr, resultCleanupErr); cleanupErr != nil {
		return LifecycleResult{}, fmt.Errorf("remove native runtime control files: %w", cleanupErr)
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var result devcontainerruntime.Result
	if err := decoder.Decode(&result); err != nil {
		return LifecycleResult{}, fmt.Errorf("decode native runtime result: %w", errors.Join(execErr, err))
	}
	if result.Version != devcontainerruntime.FormatVersion {
		return LifecycleResult{}, fmt.Errorf("native runtime result version is invalid")
	}
	if result.Error != "" {
		failure := &RuntimeFailureError{Kind: RuntimeFailureUnrecoverable, Message: result.Error}
		if result.Recoverable {
			failure.Kind = RuntimeFailureRecoverable
		}
		return LifecycleResult{}, failure
	}
	if execErr != nil {
		return LifecycleResult{}, fmt.Errorf("execute native runtime: %w", execErr)
	}
	if result.Environment == nil {
		return LifecycleResult{}, fmt.Errorf("native runtime result environment is missing")
	}
	if err := result.Environment.Validate(); err != nil {
		return LifecycleResult{}, err
	}
	state, err := json.Marshal(result.Environment)
	if err != nil {
		return LifecycleResult{}, err
	}
	if err := p.writeRuntimeFile(ctx, instanceName, runtimeEnvironmentFile, string(state), 0o600, "file"); err != nil {
		return LifecycleResult{}, err
	}
	p.writeLifecycleLog(ctx, request.LogSink, action+" Dev Container environment completed")
	return LifecycleResult{Environment: *result.Environment}, nil
}

func (p *IncusProvisioner) deleteRuntimeControlFile(instanceName, path string) error {
	if err := p.client.DeleteInstanceFile(instanceName, path); err != nil && !isNotFoundError(err) {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	return nil
}

func (p *IncusProvisioner) installRuntimeExecutable(ctx context.Context, instanceName string) error {
	architecture, err := p.execScriptOutput(ctx, instanceName, "uname -m", nil, "/")
	if err != nil {
		return fmt.Errorf("inspect runtime architecture: %w", err)
	}
	compatible := map[string][]string{
		"amd64": {"x86_64", "amd64"},
		"arm64": {"aarch64", "arm64"},
		"arm":   {"armv6l", "armv7l", "arm"},
		"386":   {"i386", "i486", "i586", "i686", "x86"},
	}
	if !slices.Contains(compatible[runtime.GOARCH], strings.TrimSpace(architecture)) {
		return fmt.Errorf("Manager executable architecture %s does not match runtime architecture %q", runtime.GOARCH, strings.TrimSpace(architecture))
	}
	managerExecutable := p.runtimeBinary
	if managerExecutable == "" {
		path, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate Manager executable: %w", err)
		}
		managerExecutable = path
	}
	content, err := os.Open(managerExecutable)
	if err != nil {
		return fmt.Errorf("read Manager executable: %w", err)
	}
	defer content.Close()
	if err := p.writeRuntimeFile(ctx, instanceName, runtimeExecutableDir, "", 0o755, "directory"); err != nil {
		return err
	}
	if err := p.writeRuntimeContent(ctx, instanceName, runtimeExecutable, content, 0o755, "file"); err != nil {
		return fmt.Errorf("install native runtime executable: %w", err)
	}
	return nil
}

func (p *IncusProvisioner) runtimeUserIdentity(ctx context.Context, instanceName, userName string) (uint32, uint32, string, error) {
	output, err := p.execScriptOutput(ctx, instanceName, `
set -eu
entry="$(getent passwd "$CODESPACE_USER")"
[ -n "$entry" ]
printf 'UID=%s\nGID=%s\nHOME=%s\n' "$(printf '%s' "$entry" | cut -d: -f3)" "$(printf '%s' "$entry" | cut -d: -f4)" "$(printf '%s' "$entry" | cut -d: -f6)"
`, map[string]string{"CODESPACE_USER": userName}, "/")
	if err != nil {
		return 0, 0, "", fmt.Errorf("resolve runtime user identity: %w", err)
	}
	values, err := parseEnvironmentFile(output, nil)
	if err != nil {
		return 0, 0, "", err
	}
	uid, err := parseUint32Env(values, "UID")
	if err != nil {
		return 0, 0, "", err
	}
	gid, err := parseUint32Env(values, "GID")
	if err != nil {
		return 0, 0, "", err
	}
	home := strings.TrimSpace(values["HOME"])
	if uid == 0 || gid == 0 || !filepath.IsAbs(home) {
		return 0, 0, "", fmt.Errorf("runtime user identity is invalid")
	}
	return uid, gid, home, nil
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
	if strings.TrimSpace(gitSSHPrivateKey) == "" && strings.TrimSpace(gitSSHPublicKey) == "" {
		gitSSHPrivateKey, _, err = p.readCredentialFile(ctx, instanceName, runtimeSeedGitSSHPrivateKey)
		if err != nil {
			return CredentialStatus{}, err
		}
		gitSSHPublicKey, _, err = p.readCredentialFile(ctx, instanceName, runtimeSeedGitSSHPublicKey)
		if err != nil {
			return CredentialStatus{}, err
		}
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
workspace_uid="$(stat -c %u "$workdir")"
workspace_gid="$(stat -c %g "$workdir")"
workspace_home="$(getent passwd "$workspace_uid" | cut -d: -f6)"
[ -n "$workspace_home" ] || exit 61
run_workspace_git() {
	sudo -u "#$workspace_uid" -g "#$workspace_gid" env HOME="$workspace_home" git "$@"
}
origin="$(run_workspace_git -C "$workdir" remote get-url origin)"
helper="$(run_workspace_git -C "$workdir" config --get credential.helper || true)"
global_helper="$(run_workspace_git config --global --get credential.helper || true)"
ssh_command="$(run_workspace_git -C "$workdir" config --get core.sshCommand || true)"
printf 'ORIGIN=%s\n' "$origin"
printf 'HELPER=%s\n' "$helper"
printf 'GLOBAL_HELPER=%s\n' "$global_helper"
printf 'SSH_COMMAND=%s\n' "$ssh_command"
`, map[string]string{"CODESPACE_WORKSPACE_DIR": workdir}, "/")
	if err != nil {
		return WorkspaceGitStatus{}, err
	}
	values, err := parseEnvironmentFile(output, nil)
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
workspace_uid="$(stat -c %u "$workdir")"
workspace_gid="$(stat -c %g "$workdir")"
[ "$workspace_uid" != "0" ] || exit 71
[ "$workspace_gid" != "0" ] || exit 71
workspace_home="$(getent passwd "$workspace_uid" | cut -d: -f6)"
[ -n "$workspace_home" ] || exit 72
sudo -u "#$workspace_uid" -g "#$workspace_gid" env HOME="$workspace_home" bash -euo pipefail -c '
	workdir="$1"
	[ -w "$workdir" ] || exit 73
	probe="$workdir/.gitea-codespace-health-$$"
	: > "$probe"
	rm -f "$probe"
' bash "$workdir"
`, map[string]string{"CODESPACE_WORKSPACE_DIR": workdir}, "/")
}

// CheckDevContainer verifies that the selected inner development container is running.
func (p *IncusProvisioner) CheckDevContainer(ctx context.Context, instanceName string) error {
	if strings.TrimSpace(instanceName) == "" {
		return fmt.Errorf("instance name is empty")
	}
	return p.execCommand(ctx, instanceName, []string{runtimeExecutable, "runtime", "check", "--state", runtimeEnvironmentFile}, nil, "/")
}

func workspaceGitCredentialConfigured(origin, helper, globalHelper, sshCommand string) bool {
	origin = strings.TrimSpace(origin)
	switch {
	case strings.HasPrefix(origin, "http://") || strings.HasPrefix(origin, "https://"):
		return strings.Contains(helper, "gitea-codespace-git-credential") ||
			strings.Contains(globalHelper, "gitea-codespace-git-credential")
	case origin != "":
		return strings.Contains(sshCommand, "/var/lib/gitea-codespace/bin/gitea-codespace-git-ssh") ||
			(strings.Contains(sshCommand, "/var/lib/gitea-codespace/git/id_ed25519") &&
				strings.Contains(sshCommand, "/var/lib/gitea-codespace/git/known_hosts") &&
				strings.Contains(sshCommand, "StrictHostKeyChecking=yes"))
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

// SeedRuntimeGitSSHKey writes the root-owned git SSH key seed before the key is registered in Gitea.
func (p *IncusProvisioner) SeedRuntimeGitSSHKey(ctx context.Context, instanceName string, request RuntimeGitSSHKeySeedRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(instanceName) == "" {
		return fmt.Errorf("instance name is empty")
	}
	files, err := runtimeGitSSHKeySeedFiles(request)
	if err != nil {
		return err
	}
	if err := p.ensureRuntimeCredentialSeedDir(ctx, instanceName); err != nil {
		return err
	}
	for _, file := range files {
		if err := p.writeRuntimeOwnedFile(ctx, instanceName, file.path, file.content, 0, 0, file.mode); err != nil {
			return fmt.Errorf("write %s: %w", file.path, err)
		}
	}
	return nil
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
	if err := p.ensureRuntimeCredentialSeedDir(ctx, instanceName); err != nil {
		return err
	}
	for _, file := range files {
		if err := p.writeRuntimeOwnedFile(ctx, instanceName, file.path, file.content, 0, 0, file.mode); err != nil {
			return fmt.Errorf("write %s: %w", file.path, err)
		}
	}
	return nil
}

// WriteRuntimeSecrets replaces the user-owned runtime secret file consumed by Dev Container and Gateway commands.
func (p *IncusProvisioner) WriteRuntimeSecrets(ctx context.Context, instanceName string, uid, gid uint32, secrets RuntimeSecretEnvironment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(instanceName) == "" {
		return fmt.Errorf("instance name is empty")
	}
	content, err := json.Marshal(secrets)
	if err != nil {
		return fmt.Errorf("encode runtime secrets: %w", err)
	}
	if err := p.waitInstanceFileAPI(ctx, instanceName); err != nil {
		return err
	}
	if err := p.createInstanceFile(ctx, instanceName, runtimeSecretDir, incus.InstanceFileArgs{
		UID: int64(uid), GID: int64(gid), Mode: runtimePrivateDirMode, Type: "directory", WriteMode: runtimeCredentialWriteMode,
	}); err != nil {
		return fmt.Errorf("write runtime secret directory: %w", err)
	}
	if err := p.writeRuntimeOwnedFile(ctx, instanceName, runtimeSecretFile, string(content), int64(uid), int64(gid), runtimeCredentialFileMode); err != nil {
		return fmt.Errorf("write runtime secrets: %w", err)
	}
	return nil
}

// ClearRuntimeSecrets removes the ephemeral secret values before an instance is stopped.
func (p *IncusProvisioner) ClearRuntimeSecrets(ctx context.Context, instanceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(instanceName) == "" {
		return fmt.Errorf("instance name is empty")
	}
	if err := p.waitInstanceFileAPI(ctx, instanceName); err != nil {
		return err
	}
	if err := p.client.DeleteInstanceFile(instanceName, runtimeSecretFile); err != nil && !isNotFoundError(err) {
		return fmt.Errorf("delete runtime secrets: %w", err)
	}
	return nil
}

func (p *IncusProvisioner) ensureRuntimeCredentialSeedDir(ctx context.Context, instanceName string) error {
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
	return nil
}

func runtimeGitSSHKeySeedFiles(request RuntimeGitSSHKeySeedRequest) ([]bootstrapCredentialFile, error) {
	if len(request.GitSSHPrivateKey) == 0 {
		return nil, fmt.Errorf("git ssh private key is empty")
	}
	if len(request.GitSSHPublicKey) == 0 {
		return nil, fmt.Errorf("git ssh public key is empty")
	}
	return []bootstrapCredentialFile{
		{path: runtimeSeedGitSSHPrivateKey, content: string(request.GitSSHPrivateKey), mode: runtimeCredentialFileMode},
		{path: runtimeSeedGitSSHPublicKey, content: string(request.GitSSHPublicKey), mode: 0o644},
	}, nil
}

func runtimeCredentialSeedFiles(request RuntimeCredentialSeedRequest) ([]bootstrapCredentialFile, error) {
	if strings.TrimSpace(request.CodespaceUUID) == "" {
		return nil, fmt.Errorf("codespace uuid is empty")
	}
	if strings.TrimSpace(request.GiteaToken) == "" {
		return nil, fmt.Errorf("gitea token is empty")
	}
	content := strings.TrimSpace(strings.Join(request.GitSSHKnownHosts, "\n"))
	if content != "" {
		content += "\n"
	}
	return []bootstrapCredentialFile{
		{path: runtimeSeedGiteaToken, content: request.GiteaToken, mode: runtimeCredentialFileMode},
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
	var manifest runtimeendpoint.EndpointManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode endpoint manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode endpoint manifest: trailing data")
	}
	if manifest.Version != runtimeendpoint.EndpointManifestVersion {
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

	if err := p.stopInstanceForDelete(ctx, instanceName); err != nil {
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
	if err := p.confirmInstanceDeleted(ctx, instanceName); err != nil {
		return err
	}
	return nil
}

func (p *IncusProvisioner) stopInstanceForDelete(ctx context.Context, instanceName string) error {
	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("get instance %s before delete: %w", instanceName, err)
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
		return fmt.Errorf("force stop instance %s before delete: %w", instanceName, err)
	}
	if err := operation.WaitContext(ctx); err != nil {
		cancelIncusOperationOnContextError(ctx, operation)
		return fmt.Errorf("wait force stop instance %s before delete: %w", instanceName, err)
	}
	return nil
}

func (p *IncusProvisioner) confirmInstanceDeleted(ctx context.Context, instanceName string) error {
	_, _, err := p.client.GetInstance(instanceName)
	if err == nil {
		return fmt.Errorf("delete instance %s: instance still exists", instanceName)
	}
	if isNotFoundError(err) {
		return nil
	}
	return fmt.Errorf("confirm delete instance %s: %w", instanceName, err)
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
	if environment.instanceType == api.InstanceTypeContainer {
		instanceConfig["security.nesting"] = "true"
	}
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
	if _, err := p.environmentForTag(tag); err != nil {
		return nil, err
	}
	if err := p.startInstance(ctx, instance.Name); err != nil {
		return nil, fmt.Errorf("start instance %s: %w", instance.Name, err)
	}
	host, err := p.instanceCommunicationHost(ctx, instance.Name)
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
	tag = strings.ToLower(strings.TrimSpace(tag))
	environment, ok := p.environments[tag]
	if !ok {
		return incusEnvironment{}, fmt.Errorf("incus environment %s is not configured", tag)
	}
	return environment, nil
}

func (p *IncusProvisioner) instanceCommunicationHost(ctx context.Context, instanceName string) (string, error) {
	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		return "", fmt.Errorf("get instance %s network devices: %w", instanceName, err)
	}
	deviceName, hardwareAddress, err := instanceNetworkDevice(instance, p.networkName)
	if err != nil {
		return "", fmt.Errorf("resolve instance %s communication device: %w", instanceName, err)
	}

	var lastErr error
	for attempt := 0; attempt < 60; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		state, _, err := p.client.GetInstanceState(instanceName)
		if err != nil {
			lastErr = fmt.Errorf("get instance state %s: %w", instanceName, err)
		} else {
			host, err := instanceStateCommunicationHost(state, hardwareAddress)
			if err != nil {
				return "", fmt.Errorf("resolve instance %s communication address: %w", instanceName, err)
			}
			if host != "" {
				return host, nil
			}
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
	return "", fmt.Errorf("instance %s has no global IPv4 address on Incus network %s device %s with MAC %s", instanceName, p.networkName, deviceName, hardwareAddress)
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

func instanceNetworkDevice(instance *api.Instance, networkName string) (string, string, error) {
	if instance == nil {
		return "", "", fmt.Errorf("instance response is empty")
	}
	networkName = strings.TrimSpace(networkName)
	if networkName == "" {
		return "", "", fmt.Errorf("incus network name is empty")
	}

	deviceName := ""
	var device map[string]string
	for name, candidate := range instance.ExpandedDevices {
		if candidate["type"] != "nic" || (candidate["network"] != networkName && candidate["parent"] != networkName) {
			continue
		}
		if deviceName != "" {
			return "", "", fmt.Errorf("multiple NIC devices connect to Incus network %s: %s and %s", networkName, deviceName, name)
		}
		deviceName = name
		device = candidate
	}
	if deviceName == "" {
		return "", "", fmt.Errorf("no NIC device connects to Incus network %s", networkName)
	}

	hardwareAddress := strings.TrimSpace(device["hwaddr"])
	if hardwareAddress == "" {
		hardwareAddress = strings.TrimSpace(instance.Config["volatile."+deviceName+".hwaddr"])
	}
	if hardwareAddress == "" {
		hardwareAddress = strings.TrimSpace(instance.ExpandedConfig["volatile."+deviceName+".hwaddr"])
	}
	parsed, err := net.ParseMAC(hardwareAddress)
	if err != nil {
		return "", "", fmt.Errorf("NIC device %s on Incus network %s has invalid MAC address %q", deviceName, networkName, hardwareAddress)
	}
	return deviceName, parsed.String(), nil
}

func instanceStateCommunicationHost(state *api.InstanceState, hardwareAddress string) (string, error) {
	target, err := net.ParseMAC(strings.TrimSpace(hardwareAddress))
	if err != nil {
		return "", fmt.Errorf("invalid communication device MAC address %q", hardwareAddress)
	}
	if state == nil {
		return "", nil
	}

	host := ""
	matchedInterface := ""
	for interfaceName, network := range state.Network {
		candidate, err := net.ParseMAC(strings.TrimSpace(network.Hwaddr))
		if err != nil || !bytes.Equal(candidate, target) {
			continue
		}
		if matchedInterface != "" {
			return "", fmt.Errorf("MAC address %s is reported by multiple guest interfaces: %s and %s", target, matchedInterface, interfaceName)
		}
		matchedInterface = interfaceName
		host = networkCommunicationHost(network)
	}
	return host, nil
}

func networkCommunicationHost(network api.InstanceStateNetwork) string {
	for _, address := range network.Addresses {
		if strings.EqualFold(strings.TrimSpace(address.Scope), "link") ||
			strings.EqualFold(strings.TrimSpace(address.Scope), "local") {
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(address.Address))
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		return ip.To4().String()
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
	project, etag, err := client.GetProject(projectName)
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
					"features.networks":        "false",
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
	if managedProjectFeaturesNeedUpdate(project.Config) {
		projectPut := project.Writable()
		projectPut.Config = copyStringMap(project.Config)
		applyManagedProjectFeatures(projectPut.Config)
		if err := client.UpdateProject(projectName, projectPut, etag); err != nil {
			return fmt.Errorf("update incus project %s features: %w", projectName, err)
		}
		project, _, err = client.GetProject(projectName)
		if err != nil {
			return fmt.Errorf("reload incus project %s: %w", projectName, err)
		}
	}
	if !projectFeatureEnabled(project.Config, "features.profiles") {
		return fmt.Errorf("incus project %s must enable features.profiles before it is used by codespace manager", projectName)
	}
	if projectFeatureEnabled(project.Config, "features.networks") {
		return fmt.Errorf("incus project %s must share default project networks before it is used by codespace manager", projectName)
	}
	if !projectFeatureEnabled(project.Config, "features.storage.volumes") {
		return fmt.Errorf("incus project %s must enable features.storage.volumes before it is used by codespace manager", projectName)
	}
	return nil
}

func managedProjectFeaturesNeedUpdate(config map[string]string) bool {
	return !projectFeatureEnabled(config, "features.profiles") ||
		projectFeatureEnabled(config, "features.networks") ||
		!projectFeatureEnabled(config, "features.storage.volumes")
}

func applyManagedProjectFeatures(config map[string]string) {
	config["features.profiles"] = "true"
	config["features.networks"] = "false"
	config["features.storage.volumes"] = "true"
}

func projectFeatureEnabled(config map[string]string, name string) bool {
	value := strings.TrimSpace(config[name])
	return strings.EqualFold(value, "true") || value == "1"
}

func ensureIncusManagedResources(baseClient, projectClient incus.InstanceServer, config IncusConfig) error {
	if !config.ProjectManage {
		return nil
	}
	if config.NetworkManage {
		defaultClient := withProject(baseClient, api.ProjectDefaultName)
		if err := ensureIncusNetwork(defaultClient, config.NetworkName); err != nil {
			return err
		}
	}
	return ensureIncusDefaultProfile(projectClient, config)
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
		stdoutLog = newLifecycleLogWriter(ctx, sink)
		stderrLog = newLifecycleLogWriter(ctx, sink)
		stdoutWriter = io.MultiWriter(stdout, stdoutLog)
		stderrWriter = io.MultiWriter(stderr, stderrLog)
		defer func() {
			stdoutLog.Flush()
			stderrLog.Flush()
			if flusher, ok := sink.(LifecycleLogFlusher); ok {
				_ = flusher.FlushLifecycleLog(context.WithoutCancel(ctx))
			}
		}()
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
		if sink != nil {
			return "", fmt.Errorf("wait instance command: %w", err)
		}
		return "", fmt.Errorf(
			"wait instance command: %w (stdout=%q stderr=%q)",
			err,
			stdout.String(),
			stderr.String(),
		)
	}
	if status, ok := incusOperationExitStatus(operation); ok && status != 0 {
		if sink != nil {
			return "", fmt.Errorf("instance command exited with status %d", status)
		}
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
	ctx  context.Context
	sink LifecycleLogSink
	mu   sync.Mutex
	buf  []byte
}

func newLifecycleLogWriter(ctx context.Context, sink LifecycleLogSink) *lifecycleLogWriter {
	return &lifecycleLogWriter{ctx: ctx, sink: sink}
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
	if len(line) == 0 {
		return
	}
	_ = w.sink.WriteLifecycleLog(w.ctx, string(line))
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
