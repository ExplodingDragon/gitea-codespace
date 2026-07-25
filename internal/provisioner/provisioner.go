// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// RuntimeState stores the locally observed runtime state.
type RuntimeState string

const (
	// RuntimeStateCreating means the runtime identity exists but startup is not stable yet.
	RuntimeStateCreating RuntimeState = "creating"
	// RuntimeStateRunning means the runtime is running.
	RuntimeStateRunning RuntimeState = "running"
	// RuntimeStateStopped means the runtime is stopped but still recoverable.
	RuntimeStateStopped RuntimeState = "stopped"
	// RuntimeStateFailed means the runtime identity exists but cannot be recovered.
	RuntimeStateFailed RuntimeState = "failed"
)

// Instance stores one provisioned codespace instance.
type Instance struct {
	CodespaceUUID     string
	Name              string
	RuntimeState      RuntimeState
	Workdir           string
	RepoFullName      string
	EnvironmentTag    string
	CommunicationHost string
}

// InstanceSpec stores the runtime instance shape requested by Gitea.
type InstanceSpec struct {
	CodespaceUUID  string
	Name           string
	RepoFullName   string
	EnvironmentTag string
}

// StartupAdmission reports which startup operation types may be fetched now.
type StartupAdmission struct {
	CreateAvailable bool
	ResumeAvailable bool
}

// StartupAdmissionChecker reports backend-specific startup admission.
type StartupAdmissionChecker interface {
	CheckStartupAdmission(ctx context.Context) (StartupAdmission, error)
}

// BootstrapRequest stores the codespace bootstrap inputs.
type BootstrapRequest struct {
	CodespaceUUID       string
	CodespaceName       string
	CodespaceOwnerName  string
	UserID              int64
	UserName            string
	UserDisplayName     string
	GitUserName         string
	GitUserEmail        string
	RuntimeUserName     string
	GiteaToken          string
	ServerURL           string
	RepoCloneHTTPURL    string
	RepoCloneSSHURL     string
	RepoWebURL          string
	RepoID              int64
	RepoFullName        string
	RepoName            string
	OwnerID             int64
	OwnerName           string
	OwnerType           string
	OwnerDisplayName    string
	StartRef            string
	RefType             string
	RefName             string
	CommitSHA           string
	Workdir             string
	EnvironmentTag      string
	GitProtocol         string
	RepoConfigPresent   bool
	RepoConfigPath      string
	RepoConfigContent   []byte
	RepoConfigSourceRef string
	RepoConfigSHA256    string
	Operation           ScriptOperation
	Scripts             ScriptSnapshot
	LogSink             LifecycleLogSink
}

// LifecycleLogSink receives lifecycle script output as complete text lines.
type LifecycleLogSink interface {
	WriteLifecycleLog(ctx context.Context, message string) error
}

// CredentialStatus stores the current runtime credential file state.
type CredentialStatus struct {
	GiteaTokenPresent bool
	GitSSHPrivateKey  []byte
	GitSSHPublicKey   []byte
}

// WorkspaceGitStatus stores the current workspace Git credential configuration.
type WorkspaceGitStatus struct {
	OriginURL            string
	CredentialConfigured bool
}

// RuntimeCredentialSeedRequest stores root-owned credential seed material.
type RuntimeCredentialSeedRequest struct {
	CodespaceUUID    string
	GiteaToken       string
	GitSSHPrivateKey []byte
	GitSSHPublicKey  []byte
	GitSSHKnownHosts []string
}

// RuntimeEndpointDeclaration stores one endpoint declared inside the runtime.
type RuntimeEndpointDeclaration struct {
	EndpointID     string `json:"endpoint_id"`
	Label          string `json:"label"`
	UpstreamScheme string `json:"upstream_scheme"`
	UpstreamPort   int    `json:"upstream_port"`
	Public         bool   `json:"public"`
}

// RuntimeResourceUsage stores externally observed runtime resource usage.
type RuntimeResourceUsage struct {
	CPUObserved        bool
	CPUUsedMillicores  int64
	CPULimitMillicores int64
	MemoryUsedBytes    int64
	MemoryLimitBytes   int64
	DiskUsedBytes      int64
	DiskLimitBytes     int64
	ObservedUnix       int64
}

// ScriptOperation identifies which lifecycle command is running.
type ScriptOperation string

const (
	// ScriptOperationCreate prepares a new workspace from a repository payload.
	ScriptOperationCreate ScriptOperation = "create"
	// ScriptOperationResume restores an existing workspace without repository payload.
	ScriptOperationResume ScriptOperation = "resume"
)

// ScriptConfig stores the init/start/resume script sources.
type ScriptConfig struct {
	Init   string
	Start  string
	Resume string
}

// ScriptSnapshot stores one complete lifecycle script suite fixed for an operation.
type ScriptSnapshot struct {
	Init   ScriptFileSnapshot
	Start  ScriptFileSnapshot
	Resume ScriptFileSnapshot
}

// ScriptFileSnapshot stores one script and its content digest.
type ScriptFileSnapshot struct {
	Content string
	SHA256  string
}

// SystemIdentity stores the non-root identity produced by init.
type SystemIdentity struct {
	UID       uint32
	GID       uint32
	SharedEnv map[string]string
}

// WorkspaceStatus stores the workspace path produced by prepare.
type WorkspaceStatus struct {
	Workdir   string
	SharedEnv map[string]string
}

// RuntimeAccess stores the shared environment produced by activate.
type RuntimeAccess struct {
	SharedEnv map[string]string
}

// WorkspaceCommandRequest stores one user command or shell opened through the Gateway.
type WorkspaceCommandRequest struct {
	InstanceName string
	Workdir      string
	Command      string
	Interactive  bool
	Cols         int
	Rows         int
}

// WorkspaceSFTPRequest stores one SFTP subsystem opened through the Gateway.
type WorkspaceSFTPRequest struct {
	InstanceName string
	Workdir      string
}

// WorkspaceCommandSession stores the live streams for one Gateway workspace session.
type WorkspaceCommandSession interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Resize(cols, rows int) error
	Wait() error
	Close() error
}

// WorkspaceCommandExitError reports the exit status returned by the runtime command.
type WorkspaceCommandExitError struct {
	Status int
}

func (e *WorkspaceCommandExitError) Error() string {
	return "workspace command exited with status " + strconv.Itoa(e.Status)
}

// BootstrapConfig stores runtime bootstrap execution settings.
type BootstrapConfig struct {
	Shell    string
	HomeDir  string
	UserName string
	User     uint32
	Group    uint32
}

// Provisioner creates and manages codespace instances.
type Provisioner interface {
	CreateOrStart(ctx context.Context, spec InstanceSpec) (*Instance, error)
	StartExisting(ctx context.Context, spec InstanceSpec) (*Instance, error)
	ListInstances(ctx context.Context) ([]*Instance, error)
	CheckCredentials(ctx context.Context, instanceName string) (CredentialStatus, error)
	SeedRuntimeCredentials(ctx context.Context, instanceName string, request RuntimeCredentialSeedRequest) error
	ReadEndpointManifest(ctx context.Context, instanceName string) ([]RuntimeEndpointDeclaration, error)
	RuntimeResourceUsage(ctx context.Context, instanceName string) (RuntimeResourceUsage, error)
	InitializeSystem(ctx context.Context, instanceName string, request BootstrapRequest) (SystemIdentity, error)
	PrepareWorkspace(ctx context.Context, instanceName string, request BootstrapRequest) (WorkspaceStatus, error)
	ActivateRuntime(ctx context.Context, instanceName string, request BootstrapRequest) (RuntimeAccess, error)
	Stop(ctx context.Context, instanceName string) error
	Delete(ctx context.Context, instanceName string) error
}

func repoDirName(repoFullName string) string {
	repoFullName = strings.Trim(repoFullName, "/")
	if repoFullName == "" {
		return "repo"
	}
	return filepath.Base(repoFullName)
}
