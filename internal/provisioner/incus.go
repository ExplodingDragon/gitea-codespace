// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

const defaultCodespaceRoot = "/codespace"
const defaultCommunicationInterface = "eth0"

const (
	runtimeCredentialDir       = "/var/lib/gitea-codespace"
	runtimeGitCredentialDir    = "/var/lib/gitea-codespace/git"
	runtimeGiteaTokenFilePath  = "/var/lib/gitea-codespace/gitea-token"
	runtimeAPITokenFilePath    = "/var/lib/gitea-codespace/runtime-token"
	runtimeCredentialDirMode   = 0o700
	runtimeCredentialFileMode  = 0o600
	runtimeCredentialWriteMode = "overwrite"
)

const (
	incusConfigManagerID     = "user.gitea.manager_id"
	incusConfigCodespaceUUID = "user.gitea.codespace_uuid"
	incusConfigSchemaVersion = "user.gitea.schema_version"
	incusConfigTag           = "user.gitea.tag"
)

type bootstrapCredentialFile struct {
	path    string
	content string
	mode    int
	kind    string
}

// IncusConfig configures one Incus-backed provisioner.
type IncusConfig struct {
	ManagerID              int64
	Project                string
	Remote                 string
	UnixSocket             string
	CodespaceRoot          string
	CommunicationInterface string
	Bootstrap              BootstrapConfig
}

// IncusProvisioner provisions codespace as Incus instances.
type IncusProvisioner struct {
	client                 incus.InstanceServer
	managerID              string
	codespaceRoot          string
	communicationInterface string
	bootstrap              BootstrapConfig
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

	codespaceRoot := config.CodespaceRoot
	if codespaceRoot == "" {
		codespaceRoot = defaultCodespaceRoot
	}
	communicationInterface := strings.TrimSpace(config.CommunicationInterface)
	if communicationInterface == "" {
		communicationInterface = defaultCommunicationInterface
	}

	return &IncusProvisioner{
		client:                 client,
		managerID:              fmt.Sprintf("%d", config.ManagerID),
		codespaceRoot:          codespaceRoot,
		communicationInterface: communicationInterface,
		bootstrap:              config.Bootstrap,
	}, nil
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
		if err := p.createInstance(ctx, spec); err != nil {
			return nil, fmt.Errorf("create instance %s: %w", instanceName, err)
		}
		instance, _, err = p.client.GetInstance(instanceName)
		if err != nil {
			return nil, fmt.Errorf("reload instance %s: %w", instanceName, err)
		}
	}

	return p.startExistingInstance(ctx, spec, instance.Name)
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
	return p.startExistingInstance(ctx, spec, instance.Name)
}

// Bootstrap clones the repo, configures git auth, and runs the init script.
func (p *IncusProvisioner) Bootstrap(ctx context.Context, instanceName string, request BootstrapRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" {
		return fmt.Errorf("instance name is empty")
	}
	if request.Workdir == "" {
		return fmt.Errorf("workdir is empty")
	}
	repoURL := request.RepoCloneHTTPURL
	if strings.EqualFold(request.GitProtocol, "GIT_PROTOCOL_SSH") && request.RepoCloneSSHURL != "" {
		repoURL = request.RepoCloneSSHURL
	}
	if repoURL == "" {
		return fmt.Errorf("repo clone url is empty")
	}

	authPrefix, httpsPrefix, err := buildGitURLPrefixes(repoURL, "codespace", request.GiteaToken)
	if err != nil {
		return fmt.Errorf("build git url prefixes: %w", err)
	}
	environment := map[string]string{
		"HOME":                       p.bootstrap.HomeDir,
		"CODESPACE_ID":               request.CodespaceUUID,
		"CODESPACE_ROOT":             request.Workdir,
		"CODESPACE_DIR":              request.Workdir,
		"CODESPACE_PARENT":           filepath.Dir(request.Workdir),
		"CODESPACE_MANAGER_BASE_URL": request.RuntimeAPIBaseURL,
		"CODESPACE_RUNTIME_TOKEN":    request.RuntimeToken,
		"GITEA_TOKEN":                request.GiteaToken,
		"REPO_URL":                   repoURL,
		"REPO_FULL_NAME":             request.RepoFullName,
		"START_REF":                  request.StartRef,
		"START_SHA":                  request.CommitSHA,
		"GIT_AUTH_PREFIX":            authPrefix,
		"GIT_HTTPS_PREFIX":           httpsPrefix,
	}

	script := strings.TrimSpace(`
set -eu
mkdir -p "$HOME" "$CODESPACE_PARENT"
if [ ! -d "$CODESPACE_DIR/.git" ]; then
  git clone "$REPO_URL" "$CODESPACE_DIR"
fi
git -C "$CODESPACE_DIR" remote set-url origin "$REPO_URL"
if [ -n "$GIT_AUTH_PREFIX" ] && [ -n "$GIT_HTTPS_PREFIX" ]; then
  git config --global credential.helper store
  git config --global url."$GIT_AUTH_PREFIX".insteadOf "$GIT_HTTPS_PREFIX"
fi
if [ -n "$START_REF" ]; then
  git -C "$CODESPACE_DIR" fetch origin "$START_REF" --tags --prune
else
  git -C "$CODESPACE_DIR" fetch --all --tags --prune
fi
if [ -n "$START_SHA" ]; then
  git -C "$CODESPACE_DIR" checkout --detach "$START_SHA"
elif [ -n "$START_REF" ]; then
  git -C "$CODESPACE_DIR" checkout --detach FETCH_HEAD
fi
`)

	if err := p.execScript(ctx, instanceName, script, environment, request.Workdir); err != nil {
		return fmt.Errorf("bootstrap instance %s: %w", instanceName, err)
	}
	return nil
}

// WriteCredentials writes the current Gitea and Runtime API tokens into the instance.
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
	if strings.TrimSpace(request.RuntimeToken) == "" {
		return fmt.Errorf("runtime token is empty")
	}
	for _, file := range bootstrapCredentialFiles(request) {
		args := incus.InstanceFileArgs{
			Content:   strings.NewReader(file.content),
			UID:       int64(p.bootstrap.User),
			GID:       int64(p.bootstrap.Group),
			Mode:      file.mode,
			Type:      file.kind,
			WriteMode: runtimeCredentialWriteMode,
		}
		if err := p.client.CreateInstanceFile(instanceName, file.path, args); err != nil {
			return fmt.Errorf("write %s: %w", file.path, err)
		}
	}
	return nil
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
		{
			path:    runtimeAPITokenFilePath,
			content: request.RuntimeToken,
			mode:    runtimeCredentialFileMode,
			kind:    "file",
		},
	}
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
		return fmt.Errorf("wait delete instance %s: %w", instanceName, err)
	}
	return nil
}

func (p *IncusProvisioner) createInstance(ctx context.Context, spec InstanceSpec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request := api.InstancesPost{
		Name: spec.Name,
		Type: api.InstanceType("container"),
		InstancePut: api.InstancePut{
			Config: map[string]string{
				incusConfigManagerID:     p.managerID,
				incusConfigCodespaceUUID: spec.CodespaceUUID,
				incusConfigSchemaVersion: "1",
				incusConfigTag:           spec.RepoTag,
			},
		},
		Source: api.InstanceSource{
			Type:     "image",
			Alias:    trimRemoteAlias("images:debian/12"),
			Server:   imageServerForAlias("images:debian/12"),
			Protocol: "simplestreams",
		},
	}

	operation, err := p.client.CreateInstance(request)
	if err != nil {
		return fmt.Errorf("create instance request: %w", err)
	}
	if err := operation.WaitContext(ctx); err != nil {
		return fmt.Errorf("wait create instance: %w", err)
	}
	return nil
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

// ResolveRuntimeSource matches one Runtime API source address to an owned running Incus instance.
func (p *IncusProvisioner) ResolveRuntimeSource(ctx context.Context, sourceIP string) (RuntimeSource, bool, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeSource{}, false, err
	}
	parsedIP := net.ParseIP(strings.TrimSpace(sourceIP))
	if parsedIP == nil {
		return RuntimeSource{}, false, nil
	}
	instances, err := p.client.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return RuntimeSource{}, false, fmt.Errorf("list instances: %w", err)
	}

	var result RuntimeSource
	matched := false
	for _, instance := range instances {
		owned, ok := p.instanceFromAPI(instance)
		if !ok || owned.RuntimeState != RuntimeStateRunning {
			continue
		}
		state, _, err := p.client.GetInstanceState(instance.Name)
		if err != nil {
			if isNotFoundError(err) {
				continue
			}
			return RuntimeSource{}, false, fmt.Errorf("get instance state %s: %w", instance.Name, err)
		}
		if !instanceStateHasSourceIP(state, parsedIP, p.communicationInterface) {
			continue
		}
		if matched {
			return RuntimeSource{}, false, nil
		}
		result = RuntimeSource{
			CodespaceUUID: owned.CodespaceUUID,
			InstanceName:  owned.Name,
		}
		matched = true
	}
	return result, matched, nil
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

func instanceStateHasSourceIP(state *api.InstanceState, sourceIP net.IP, interfaceName string) bool {
	if state == nil || sourceIP == nil {
		return false
	}
	if strings.TrimSpace(interfaceName) != "" {
		return networkAddressHasSourceIP(state.Network[interfaceName], sourceIP)
	}
	for _, network := range state.Network {
		if networkAddressHasSourceIP(network, sourceIP) {
			return true
		}
	}
	return false
}

func networkAddressHasSourceIP(network api.InstanceStateNetwork, sourceIP net.IP) bool {
	for _, address := range network.Addresses {
		if strings.EqualFold(strings.TrimSpace(address.Scope), "link") ||
			strings.EqualFold(strings.TrimSpace(address.Scope), "local") {
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(address.Address))
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		if ip.Equal(sourceIP) {
			return true
		}
	}
	return false
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
		return fmt.Errorf("wait start instance: %w", err)
	}
	return nil
}

func (p *IncusProvisioner) startExistingInstance(ctx context.Context, spec InstanceSpec, instanceName string) (*Instance, error) {
	if err := p.startInstance(ctx, instanceName); err != nil {
		return nil, fmt.Errorf("start instance %s: %w", instanceName, err)
	}
	return &Instance{
		CodespaceUUID:          spec.CodespaceUUID,
		Name:                   instanceName,
		RuntimeState:           RuntimeStateRunning,
		Workdir:                filepath.Join(p.codespaceRoot, repoDirName(spec.RepoFullName)),
		RepoFullName:           spec.RepoFullName,
		RepoTag:                spec.RepoTag,
		InternalSSHHost:        instanceName,
		InternalSSHPort:        22,
		InternalSSHUser:        "root",
		InternalSSHAuthMode:    "publickey",
		InternalSSHFingerprint: "SHA256:incus-local",
	}, nil
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

func trimRemoteAlias(value string) string {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return value
}

func imageServerForAlias(value string) string {
	if strings.HasPrefix(value, "images:") {
		return "https://images.linuxcontainers.org"
	}
	return ""
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
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	operation, err := p.client.ExecInstance(instanceName, api.InstanceExecPost{
		Command:     []string{p.bootstrap.Shell, "-lc", script},
		Environment: environment,
		Cwd:         workdir,
		User:        p.bootstrap.User,
		Group:       p.bootstrap.Group,
	}, &incus.InstanceExecArgs{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return fmt.Errorf("exec bootstrap command: %w", err)
	}
	if err := operation.WaitContext(ctx); err != nil {
		return fmt.Errorf(
			"wait bootstrap command: %w (stdout=%q stderr=%q)",
			err,
			stdout.String(),
			stderr.String(),
		)
	}
	return nil
}

func isNotFoundError(err error) bool {
	var apiStatus api.StatusError
	return errors.As(err, &apiStatus) && apiStatus.Status() == 404
}
