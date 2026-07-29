// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"gitea.dev/codespace/internal/devcontainer"
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

const (
	DevContainerSourcePlatformDefault = "platform_default"
	DevContainerSourceRepository      = "repository"
	// WorkspaceIDEPort is reserved for the platform-managed code-server process.
	WorkspaceIDEPort = 13337
	// WorkspaceIDEPortEnv persists the ready code-server port in runtime state.
	WorkspaceIDEPortEnv = "CODESPACE_WEB_IDE_PORT"
)

// DevContainerConfiguration identifies the selected internal development environment.
type DevContainerConfiguration struct {
	Source        string
	Path          string
	CommitSHA     string
	ContentSHA256 string
	DefaultImage  string
}

// Validate checks source-specific Dev Container fields before provisioning.
func (config DevContainerConfiguration) Validate() error {
	switch strings.TrimSpace(config.Source) {
	case DevContainerSourcePlatformDefault:
		if strings.TrimSpace(config.DefaultImage) == "" {
			return fmt.Errorf("platform default image is required")
		}
		if config.Path != "" || config.CommitSHA != "" || config.ContentSHA256 != "" {
			return fmt.Errorf("platform default contains repository fields")
		}
	case DevContainerSourceRepository:
		configPath := strings.TrimSpace(config.Path)
		if configPath == "" || configPath == "." || configPath == ".." || path.IsAbs(configPath) || path.Clean(configPath) != configPath || strings.HasPrefix(configPath, "../") {
			return fmt.Errorf("repository path is invalid")
		}
		commitSHA := strings.TrimSpace(config.CommitSHA)
		if (len(commitSHA) != 40 && len(commitSHA) != 64) || !validHex(commitSHA) {
			return fmt.Errorf("repository commit SHA is invalid")
		}
		contentSHA256 := strings.TrimSpace(config.ContentSHA256)
		if len(contentSHA256) != 64 || !validHex(contentSHA256) {
			return fmt.Errorf("repository content SHA256 is invalid")
		}
		if config.DefaultImage != "" {
			return fmt.Errorf("repository configuration contains a default image")
		}
	default:
		return fmt.Errorf("source is invalid")
	}
	return nil
}

func validHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

// LifecycleRequest stores the inputs for one native runtime transition.
type LifecycleRequest struct {
	CodespaceUUID    string
	CodespaceName    string
	UserName         string
	GitUserEmail     string
	RuntimeUserName  string
	GiteaToken       string
	ServerURL        string
	RepoCloneHTTPURL string
	RepoCloneSSHURL  string
	RepoFullName     string
	StartRef         string
	CommitSHA        string
	Workdir          string
	EnvironmentTag   string
	GitProtocol      string
	DevContainer     DevContainerConfiguration
	Environment      *devcontainer.Environment
	OperationVersion int64
	Operation        LifecycleOperation
	LogSink          LifecycleLogSink
}

// RuntimeEnvironment is the complete outer identity and inner Dev Container target.
type RuntimeEnvironment struct {
	User        uint32                   `json:"user"`
	Group       uint32                   `json:"group"`
	Environment devcontainer.Environment `json:"environment"`
}

func (environment RuntimeEnvironment) Validate() error {
	if environment.User == 0 || environment.Group == 0 {
		return fmt.Errorf("runtime user and group are required")
	}
	return environment.Environment.Validate()
}

// LifecycleLogSink receives lifecycle output as complete text lines.
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
	GitSSHKnownHosts []string
}

// RuntimeSecretEnvironment stores environment variables that only live while an instance is running.
type RuntimeSecretEnvironment map[string]string

// RuntimeGitSSHKeySeedRequest stores root-owned git SSH key seed material.
type RuntimeGitSSHKeySeedRequest struct {
	GitSSHPrivateKey []byte
	GitSSHPublicKey  []byte
}

// RuntimeEndpointDeclaration stores one ordinary endpoint declared inside the runtime.
type RuntimeEndpointDeclaration = devcontainer.Endpoint

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

// LifecycleOperation identifies which lifecycle transition is running.
type LifecycleOperation string

const (
	// LifecycleOperationCreate prepares a new workspace from a repository payload.
	LifecycleOperationCreate LifecycleOperation = "create"
	// LifecycleOperationResume starts an existing workspace without repository payload.
	LifecycleOperationResume LifecycleOperation = "resume"
	// LifecycleOperationStop stops a running workspace.
	LifecycleOperationStop LifecycleOperation = "stop"
)

// SystemIdentity stores the non-root identity and workspace produced by bootstrap.
type SystemIdentity struct {
	UID       uint32
	GID       uint32
	Workspace string
	Home      string
}

// LifecycleResult stores the complete Dev Container environment after a transition.
type LifecycleResult struct {
	Environment devcontainer.Environment
}

// WorkspaceCommandRequest stores one user command or shell opened through the Gateway.
type WorkspaceCommandRequest struct {
	InstanceName string
	Command      string
	Interactive  bool
	Cols         int
	Rows         int
}

// WorkspaceSFTPRequest stores one SFTP subsystem opened through the Gateway.
type WorkspaceSFTPRequest struct {
	InstanceName string
	Workdir      string
	User         uint32
	Group        uint32
}

// WorkspaceCommandSession stores the live streams for one Gateway workspace session.
type WorkspaceCommandSession interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Resize(cols, rows int) error
	Signal(signal int) error
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
	SeedRuntimeGitSSHKey(ctx context.Context, instanceName string, request RuntimeGitSSHKeySeedRequest) error
	SeedRuntimeCredentials(ctx context.Context, instanceName string, request RuntimeCredentialSeedRequest) error
	WriteRuntimeSecrets(ctx context.Context, instanceName string, uid, gid uint32, secrets RuntimeSecretEnvironment) error
	ClearRuntimeSecrets(ctx context.Context, instanceName string) error
	ReadEndpointManifest(ctx context.Context, instanceName string) ([]RuntimeEndpointDeclaration, error)
	RuntimeResourceUsage(ctx context.Context, instanceName string) (RuntimeResourceUsage, error)
	BootstrapSystem(ctx context.Context, instanceName string, request LifecycleRequest) (SystemIdentity, error)
	StartEnvironment(ctx context.Context, instanceName string, request LifecycleRequest) (LifecycleResult, error)
	StopEnvironment(ctx context.Context, instanceName string, request LifecycleRequest) (LifecycleResult, error)
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
