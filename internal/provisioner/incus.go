// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
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
const defaultVMAgentWaitTimeout = 2 * time.Minute
const maxIncusExecCapturedOutput = 1024 * 1024

const (
	runtimeCredentialDir       = "/var/lib/gitea-codespace"
	runtimeGitCredentialDir    = "/var/lib/gitea-codespace/git"
	runtimeManifestDir         = "/var/lib/gitea-codespace/runtime"
	runtimeGiteaTokenFilePath  = "/var/lib/gitea-codespace/gitea-token"
	runtimeEndpointManifest    = "/var/lib/gitea-codespace/runtime/endpoints.json"
	runtimeGitSSHPrivateKey    = "/var/lib/gitea-codespace/git/id_ed25519"
	runtimeGitSSHPublicKey     = "/var/lib/gitea-codespace/git/id_ed25519.pub"
	runtimeGitSSHKnownHosts    = "/var/lib/gitea-codespace/git/known_hosts"
	runtimeSharedEnvFilePath   = "/var/lib/gitea-codespace/env"
	runtimeScriptResultDir     = "/var/lib/gitea-codespace/results"
	runtimeScriptParentDir     = "/usr/local/libexec"
	runtimeScriptDir           = "/usr/local/libexec/gitea-codespace"
	runtimeCredentialDirMode   = 0o700
	runtimeCredentialFileMode  = 0o600
	runtimeCredentialWriteMode = "overwrite"
)

const (
	incusConfigManagerID     = "user.gitea.manager_id"
	incusConfigCodespaceUUID = "user.gitea.codespace_uuid"
	incusConfigSchemaVersion = "user.gitea.schema_version"
	incusConfigTag           = "user.gitea.tag"
	incusConfigE2ERunID      = "user.gitea.e2e_run_id"
)

type bootstrapCredentialFile struct {
	path    string
	content string
	mode    int
	kind    string
}

type runtimeEndpointManifestFile struct {
	Version   int                          `json:"version"`
	Endpoints []RuntimeEndpointDeclaration `json:"endpoints"`
}

// IncusConfig configures one Incus-backed provisioner.
type IncusConfig struct {
	ManagerID     int64
	Project       string
	Remote        string
	UnixSocket    string
	Templates     map[string]IncusTemplateConfig
	ExtraConfig   map[string]string
	CodespaceRoot string
	Bootstrap     BootstrapConfig
	Scripts       ScriptConfig
}

// IncusTemplateConfig stores one Incus template selected by repository tag.
type IncusTemplateConfig struct {
	Image                  string
	InstanceType           string
	CPU                    int32
	MemoryLimit            string
	RootDiskSize           string
	Profiles               []string
	CommunicationInterface string
}

// IncusProvisioner provisions codespace as Incus instances.
type IncusProvisioner struct {
	client        incus.InstanceServer
	managerID     string
	project       string
	templates     map[string]incusTemplate
	extraConfig   map[string]string
	codespaceRoot string
	bootstrap     BootstrapConfig
	scripts       ScriptSnapshot
	mu            sync.Mutex
	cpuSamples    map[string]incusCPUSample
}

type incusTemplate struct {
	image                  string
	instanceType           api.InstanceType
	cpu                    int32
	memoryLimit            string
	rootDiskSize           string
	profiles               []string
	communicationInterface string
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
	client, err := connectIncus(config)
	if err != nil {
		return nil, fmt.Errorf("connect incus: %w", err)
	}
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

	codespaceRoot := config.CodespaceRoot
	if codespaceRoot == "" {
		codespaceRoot = defaultCodespaceRoot
	}
	scripts, err := LoadScripts(config.Scripts)
	if err != nil {
		return nil, err
	}
	templates, err := normalizeIncusTemplates(config)
	if err != nil {
		return nil, err
	}

	bootstrap := normalizedBootstrapConfig(config.Bootstrap)

	return &IncusProvisioner{
		client:        client,
		managerID:     fmt.Sprintf("%d", config.ManagerID),
		project:       project,
		templates:     templates,
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
	for tag, template := range p.templates {
		templateAdmission, err := incusTemplateStartupAdmission(state, template)
		if err != nil {
			return StartupAdmission{}, fmt.Errorf("check template %s startup admission: %w", tag, err)
		}
		if !templateAdmission.CreateAvailable {
			admission.CreateAvailable = false
		}
	}
	return admission, nil
}

func incusTemplateStartupAdmission(state *api.ProjectState, template incusTemplate) (StartupAdmission, error) {
	admission := StartupAdmission{ResumeAvailable: true, CreateAvailable: true}
	if state == nil || len(state.Resources) == 0 {
		return admission, nil
	}
	if !projectResourceAvailable(state.Resources, "instances", 1) {
		admission.CreateAvailable = false
	}
	switch template.instanceType {
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
		memoryLimit := strings.TrimSpace(template.memoryLimit)
		if memoryLimit == "" {
			admission.CreateAvailable = false
			return admission, nil
		}
		required, err := units.ParseByteSizeString(memoryLimit)
		if err != nil {
			return StartupAdmission{}, fmt.Errorf("parse incus template memory %q: %w", memoryLimit, err)
		}
		if memory.Usage+required > memory.Limit {
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

func normalizeIncusTemplates(config IncusConfig) (map[string]incusTemplate, error) {
	if len(config.Templates) == 0 {
		return nil, fmt.Errorf("incus templates are required")
	}
	normalized := make(map[string]incusTemplate, len(config.Templates))
	for tag, template := range config.Templates {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, fmt.Errorf("incus template tag is empty")
		}
		image := strings.TrimSpace(template.Image)
		if image == "" {
			return nil, fmt.Errorf("incus template %s image is required", tag)
		}
		instanceType, err := normalizeIncusInstanceType(template.InstanceType)
		if err != nil {
			return nil, fmt.Errorf("incus template %s: %w", tag, err)
		}
		communicationInterface := strings.TrimSpace(template.CommunicationInterface)
		if communicationInterface == "" {
			return nil, fmt.Errorf("incus template %s communication_nic is required", tag)
		}
		if template.CPU < 1 {
			return nil, fmt.Errorf("incus template %s cpu must be positive", tag)
		}
		if strings.TrimSpace(template.MemoryLimit) == "" {
			return nil, fmt.Errorf("incus template %s memory is required", tag)
		}
		if strings.TrimSpace(template.RootDiskSize) == "" {
			return nil, fmt.Errorf("incus template %s root_disk_size is required", tag)
		}
		if len(template.Profiles) == 0 {
			return nil, fmt.Errorf("incus template %s profiles are required", tag)
		}
		normalized[tag] = incusTemplate{
			image:                  image,
			instanceType:           instanceType,
			cpu:                    template.CPU,
			memoryLimit:            strings.TrimSpace(template.MemoryLimit),
			rootDiskSize:           strings.TrimSpace(template.RootDiskSize),
			profiles:               normalizedIncusProfiles(template.Profiles),
			communicationInterface: communicationInterface,
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
	case "container":
		return api.InstanceTypeContainer, nil
	case "virtual-machine":
		return api.InstanceTypeVM, nil
	default:
		return "", fmt.Errorf("incus instance_type must be container or virtual-machine")
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
		template, err := p.templateForTag(spec.RepoTag)
		if err != nil {
			return nil, err
		}
		if err := p.createInstance(ctx, spec, template); err != nil {
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
	return CredentialStatus{
		GiteaTokenPresent: giteaPresent && strings.TrimSpace(giteaToken) != "",
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

// WriteCredentials writes the current Gitea token into the instance.
func (p *IncusProvisioner) WriteCredentials(ctx context.Context, instanceName string, request CredentialRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" {
		return fmt.Errorf("instance name is empty")
	}
	if strings.TrimSpace(request.GiteaToken) == "" {
		return fmt.Errorf("gitea token is empty")
	}
	for _, file := range bootstrapCredentialFiles(request) {
		args := incus.InstanceFileArgs{
			Content:   strings.NewReader(file.content),
			UID:       credentialFileID(request.UID, p.bootstrap.User),
			GID:       credentialFileID(request.GID, p.bootstrap.Group),
			Mode:      file.mode,
			Type:      file.kind,
			WriteMode: runtimeCredentialWriteMode,
		}
		if err := p.createInstanceFile(ctx, instanceName, file.path, args); err != nil {
			return fmt.Errorf("write %s: %w", file.path, err)
		}
	}
	return nil
}

// EnsureGitSSHKey creates or reads the runtime Git SSH key pair and returns the public key.
func (p *IncusProvisioner) EnsureGitSSHKey(ctx context.Context, instanceName string, request GitSSHKeyRequest) (GitSSHKey, error) {
	if err := ctx.Err(); err != nil {
		return GitSSHKey{}, err
	}
	if strings.TrimSpace(instanceName) == "" {
		return GitSSHKey{}, fmt.Errorf("instance name is empty")
	}
	uid := credentialFileID(request.UID, p.bootstrap.User)
	gid := credentialFileID(request.GID, p.bootstrap.Group)
	output, err := p.execScriptOutput(ctx, instanceName, `
set -eu
install -d -m 700 -o "$CODESPACE_UID" -g "$CODESPACE_GID" /var/lib/gitea-codespace/git
if [ ! -f /var/lib/gitea-codespace/git/id_ed25519 ]; then
  sudo -u "#$CODESPACE_UID" ssh-keygen -t ed25519 -N '' -f /var/lib/gitea-codespace/git/id_ed25519 >/dev/null
fi
chown "$CODESPACE_UID:$CODESPACE_GID" /var/lib/gitea-codespace/git/id_ed25519 /var/lib/gitea-codespace/git/id_ed25519.pub
chmod 600 /var/lib/gitea-codespace/git/id_ed25519
chmod 644 /var/lib/gitea-codespace/git/id_ed25519.pub
cat /var/lib/gitea-codespace/git/id_ed25519.pub
`, map[string]string{
		"CODESPACE_UID": fmt.Sprintf("%d", uid),
		"CODESPACE_GID": fmt.Sprintf("%d", gid),
	}, "/")
	if err != nil {
		return GitSSHKey{}, fmt.Errorf("ensure git ssh key: %w", err)
	}
	publicKey := strings.TrimSpace(output)
	if publicKey == "" {
		return GitSSHKey{}, fmt.Errorf("git ssh public key is empty")
	}
	return GitSSHKey{PublicKey: publicKey}, nil
}

// WriteGitSSHKnownHosts writes trusted Git SSH host keys into the runtime.
func (p *IncusProvisioner) WriteGitSSHKnownHosts(ctx context.Context, instanceName string, lines []string, request GitSSHKeyRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(instanceName) == "" {
		return fmt.Errorf("instance name is empty")
	}
	content := strings.TrimSpace(strings.Join(lines, "\n"))
	if content != "" {
		content += "\n"
	}
	args := incus.InstanceFileArgs{
		Content:   strings.NewReader(content),
		UID:       credentialFileID(request.UID, p.bootstrap.User),
		GID:       credentialFileID(request.GID, p.bootstrap.Group),
		Mode:      runtimeCredentialFileMode,
		Type:      "file",
		WriteMode: runtimeCredentialWriteMode,
	}
	if err := p.createInstanceFile(ctx, instanceName, runtimeGitSSHKnownHosts, args); err != nil {
		return fmt.Errorf("write %s: %w", runtimeGitSSHKnownHosts, err)
	}
	return nil
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

func bootstrapCredentialFiles(request CredentialRequest) []bootstrapCredentialFile {
	return []bootstrapCredentialFile{
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
			content: request.GiteaToken,
			mode:    runtimeCredentialFileMode,
			kind:    "file",
		},
	}
}

func credentialFileID(value, fallback uint32) int64 {
	if value != 0 {
		return int64(value)
	}
	return int64(fallback)
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

func (p *IncusProvisioner) createInstance(ctx context.Context, spec InstanceSpec, template incusTemplate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	instanceConfig := map[string]string{
		incusConfigManagerID:     p.managerID,
		incusConfigCodespaceUUID: spec.CodespaceUUID,
		incusConfigSchemaVersion: "1",
		incusConfigTag:           spec.RepoTag,
	}
	for key, value := range p.extraConfig {
		key = strings.TrimSpace(key)
		if key != "" {
			instanceConfig[key] = value
		}
	}
	rootDiskName := ""
	rootDiskDevice := map[string]string(nil)
	if template.rootDiskSize != "" {
		var err error
		rootDiskName, rootDiskDevice, err = p.rootDiskDevice(template.profiles)
		if err != nil {
			return err
		}
	}
	request := incusCreateRequest(spec, template, rootDiskName, rootDiskDevice, instanceConfig)

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

func incusCreateRequest(spec InstanceSpec, template incusTemplate, rootDiskName string, rootDiskDevice map[string]string, instanceConfig map[string]string) api.InstancesPost {
	if template.memoryLimit != "" {
		instanceConfig["limits.memory"] = template.memoryLimit
	}
	if template.cpu > 0 {
		instanceConfig["limits.cpu"] = fmt.Sprintf("%d", template.cpu)
	}
	instanceDevices := map[string]map[string]string{}
	if template.rootDiskSize != "" {
		device := copyStringMap(rootDiskDevice)
		device["type"] = "disk"
		device["path"] = "/"
		device["size"] = template.rootDiskSize
		instanceDevices[rootDiskName] = device
	}
	return api.InstancesPost{
		Name: spec.Name,
		Type: template.instanceType,
		InstancePut: api.InstancePut{
			Config:   instanceConfig,
			Devices:  instanceDevices,
			Profiles: append([]string(nil), template.profiles...),
		},
		Source: api.InstanceSource{
			Type:        "image",
			Alias:       incusImageAlias(template.image),
			Fingerprint: incusImageFingerprint(template.image),
			Server:      imageServerForAlias(template.image),
			Protocol:    imageProtocolForAlias(template.image),
		},
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
		CodespaceUUID: codespaceUUID,
		Name:          instance.Name,
		RuntimeState:  incusRuntimeState(instance.Status),
		RepoTag:       instance.Config[incusConfigTag],
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
	tag := strings.TrimSpace(instance.Config[incusConfigTag])
	if tag == "" {
		tag = spec.RepoTag
	}
	template, err := p.templateForTag(tag)
	if err != nil {
		return nil, err
	}
	if err := p.startInstance(ctx, instance.Name); err != nil {
		return nil, fmt.Errorf("start instance %s: %w", instance.Name, err)
	}
	host, err := p.instanceCommunicationHost(ctx, instance.Name, template.communicationInterface)
	if err != nil {
		return nil, err
	}
	return &Instance{
		CodespaceUUID:     spec.CodespaceUUID,
		Name:              instance.Name,
		RuntimeState:      RuntimeStateRunning,
		Workdir:           filepath.Join(p.codespaceRoot, repoDirName(spec.RepoFullName)),
		RepoFullName:      spec.RepoFullName,
		RepoTag:           spec.RepoTag,
		CommunicationHost: host,
	}, nil
}

func (p *IncusProvisioner) templateForTag(tag string) (incusTemplate, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = "default"
	}
	template, ok := p.templates[tag]
	if !ok {
		return incusTemplate{}, fmt.Errorf("incus template %s is not configured", tag)
	}
	return template, nil
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

func connectIncus(config IncusConfig) (incus.InstanceServer, error) {
	if config.Remote != "" {
		client, err := incus.ConnectIncus(config.Remote, nil)
		if err != nil {
			return nil, fmt.Errorf("connect remote %s: %w", config.Remote, err)
		}
		return withProject(client, config.Project), nil
	}

	client, err := incus.ConnectIncusUnix(config.UnixSocket, nil)
	if err != nil {
		return nil, fmt.Errorf("connect unix socket %q: %w", config.UnixSocket, err)
	}
	return withProject(client, config.Project), nil
}

func withProject(client incus.InstanceServer, project string) incus.InstanceServer {
	if project == "" {
		return client
	}
	return client.UseProject(project)
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
	_, err := p.execInstanceCommand(ctx, instanceName, command, environment, workdir, false)
	return err
}

func (p *IncusProvisioner) execCommandOutput(
	ctx context.Context,
	instanceName string,
	command []string,
	environment map[string]string,
	workdir string,
) (string, error) {
	return p.execInstanceCommand(ctx, instanceName, command, environment, workdir, true)
}

func (p *IncusProvisioner) execInstanceCommand(
	ctx context.Context,
	instanceName string,
	command []string,
	environment map[string]string,
	workdir string,
	requireCompleteStdout bool,
) (string, error) {
	if err := p.waitInstanceFileAPI(ctx, instanceName); err != nil {
		return "", err
	}
	stdout := newBoundedOutputBuffer(maxIncusExecCapturedOutput)
	stderr := newBoundedOutputBuffer(maxIncusExecCapturedOutput)

	operation, err := p.client.ExecInstance(instanceName, api.InstanceExecPost{
		Command:     command,
		WaitForWS:   true,
		Environment: environment,
		Cwd:         workdir,
		User:        p.bootstrap.User,
		Group:       p.bootstrap.Group,
	}, &incus.InstanceExecArgs{
		Stdout: stdout,
		Stderr: stderr,
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
		Mode:      runtimeCredentialDirMode,
		Type:      "directory",
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
