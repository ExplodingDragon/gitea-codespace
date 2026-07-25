// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// DummyProvisioner simulates backend operations for tests.
type DummyProvisioner struct {
	mu         sync.Mutex
	instances  map[string]*Instance
	tokens     map[string]string
	privateKey map[string][]byte
	publicKey  map[string][]byte
	endpoints  map[string][]RuntimeEndpointDeclaration
	knownHosts map[string][]string
}

// NewDummy creates one dummy provisioner.
func NewDummy() *DummyProvisioner {
	return &DummyProvisioner{
		instances:  make(map[string]*Instance),
		tokens:     make(map[string]string),
		privateKey: make(map[string][]byte),
		publicKey:  make(map[string][]byte),
		endpoints:  make(map[string][]RuntimeEndpointDeclaration),
		knownHosts: make(map[string][]string),
	}
}

// CreateOrStart creates or starts one instance.
func (p *DummyProvisioner) CreateOrStart(ctx context.Context, spec InstanceSpec) (*Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if spec.Name == "" {
		return nil, fmt.Errorf("instance name is empty")
	}

	instance, ok := p.instances[spec.Name]
	if !ok {
		instance = &Instance{
			CodespaceUUID:     spec.CodespaceUUID,
			Name:              spec.Name,
			RuntimeState:      RuntimeStateRunning,
			RepoFullName:      spec.RepoFullName,
			EnvironmentTag:    spec.EnvironmentTag,
			CommunicationHost: "127.0.0.1",
		}
		p.instances[instance.Name] = instance
	}
	if p.tokens[instance.Name] == "" {
		p.tokens[instance.Name] = "gcs_dummy"
	}
	instance.Workdir = "/codespace/" + repoDirName(spec.RepoFullName)
	instance.RuntimeState = RuntimeStateRunning

	copyValue := *instance
	return &copyValue, nil
}

// StartExisting starts one existing instance.
func (p *DummyProvisioner) StartExisting(ctx context.Context, spec InstanceSpec) (*Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if spec.Name == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	instance, ok := p.instances[spec.Name]
	if !ok {
		return nil, fmt.Errorf("instance %s does not exist", spec.Name)
	}
	if p.tokens[instance.Name] == "" {
		p.tokens[instance.Name] = "gcs_dummy"
	}
	instance.Workdir = "/codespace/" + repoDirName(instance.RepoFullName)
	instance.RuntimeState = RuntimeStateRunning

	copyValue := *instance
	return &copyValue, nil
}

// ListInstances returns all local dummy instances.
func (p *DummyProvisioner) ListInstances(ctx context.Context) ([]*Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	instances := make([]*Instance, 0, len(p.instances))
	for _, instance := range p.instances {
		copyValue := *instance
		instances = append(instances, &copyValue)
	}
	return instances, nil
}

// OpenWorkspaceCommand simulates one Gateway user shell or exec command.
func (p *DummyProvisioner) OpenWorkspaceCommand(ctx context.Context, request WorkspaceCommandRequest) (WorkspaceCommandSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.InstanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	if request.Workdir == "" {
		return nil, fmt.Errorf("workdir is empty")
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	session := &dummyWorkspaceCommandSession{
		stdin:    stdinWriter,
		stdout:   stdoutReader,
		stderr:   stderrReader,
		waitDone: make(chan error, 1),
	}
	go func() {
		_, _ = io.Copy(io.Discard, stdinReader)
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		session.waitDone <- nil
	}()
	if request.Command != "" {
		go func() {
			_, _ = fmt.Fprintf(stdoutWriter, "dummy command: %s\n", request.Command)
			_ = stdinWriter.Close()
		}()
	}
	return session, nil
}

// OpenWorkspaceSFTP simulates a workspace-rooted SFTP subsystem.
func (p *DummyProvisioner) OpenWorkspaceSFTP(ctx context.Context, request WorkspaceSFTPRequest) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.InstanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	if request.Workdir == "" {
		return nil, fmt.Errorf("workdir is empty")
	}
	clientConn, serverConn := net.Pipe()
	server := sftp.NewRequestServer(serverConn, sftp.InMemHandler(), sftp.WithStartDirectory("/"))
	go func() {
		_ = server.Serve()
		_ = serverConn.Close()
	}()
	return clientConn, nil
}

// CheckCredentials returns the simulated runtime credential file state.
func (p *DummyProvisioner) CheckCredentials(ctx context.Context, instanceName string) (CredentialStatus, error) {
	if err := ctx.Err(); err != nil {
		return CredentialStatus{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	return CredentialStatus{
		GiteaTokenPresent: p.tokens[instanceName] != "",
		GitSSHPrivateKey:  append([]byte(nil), p.privateKey[instanceName]...),
		GitSSHPublicKey:   append([]byte(nil), p.publicKey[instanceName]...),
	}, nil
}

// CheckWorkspaceGit returns the simulated workspace Git credential state.
func (p *DummyProvisioner) CheckWorkspaceGit(ctx context.Context, _ string, workdir string) (WorkspaceGitStatus, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceGitStatus{}, err
	}
	if workdir == "" {
		return WorkspaceGitStatus{}, fmt.Errorf("workdir is empty")
	}
	return WorkspaceGitStatus{
		OriginURL:            "https://gitea.example.com/owner/repo.git",
		CredentialConfigured: true,
	}, nil
}

// CheckWorkspaceAccess simulates a Gateway-backed workspace availability check.
func (p *DummyProvisioner) CheckWorkspaceAccess(ctx context.Context, instanceName string, workdir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" {
		return fmt.Errorf("instance name is empty")
	}
	if workdir == "" {
		return fmt.Errorf("workdir is empty")
	}
	return nil
}

// SeedRuntimeCredentials simulates writing root-owned credential seed files.
func (p *DummyProvisioner) SeedRuntimeCredentials(ctx context.Context, instanceName string, request RuntimeCredentialSeedRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" {
		return fmt.Errorf("instance name is empty")
	}
	if request.CodespaceUUID == "" {
		return fmt.Errorf("codespace uuid is empty")
	}
	if request.GiteaToken == "" {
		return fmt.Errorf("gitea token is empty")
	}
	if len(request.GitSSHPrivateKey) == 0 || len(request.GitSSHPublicKey) == 0 {
		return fmt.Errorf("git ssh key seed is empty")
	}
	if len(request.GitSSHKnownHosts) == 0 {
		return fmt.Errorf("git ssh known hosts seed is empty")
	}
	p.mu.Lock()
	p.tokens[instanceName] = request.GiteaToken
	p.privateKey[instanceName] = append([]byte(nil), request.GitSSHPrivateKey...)
	p.publicKey[instanceName] = append([]byte(nil), request.GitSSHPublicKey...)
	p.knownHosts[instanceName] = append([]string(nil), request.GitSSHKnownHosts...)
	p.mu.Unlock()
	return nil
}

// ReadEndpointManifest returns the simulated runtime endpoint manifest.
func (p *DummyProvisioner) ReadEndpointManifest(ctx context.Context, instanceName string) ([]RuntimeEndpointDeclaration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if instanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]RuntimeEndpointDeclaration(nil), p.endpoints[instanceName]...), nil
}

// RuntimeResourceUsage returns a deterministic resource sample for tests.
func (p *DummyProvisioner) RuntimeResourceUsage(ctx context.Context, instanceName string) (RuntimeResourceUsage, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeResourceUsage{}, err
	}
	if instanceName == "" {
		return RuntimeResourceUsage{}, fmt.Errorf("instance name is empty")
	}
	return RuntimeResourceUsage{
		CPUObserved:        true,
		CPUUsedMillicores:  125,
		CPULimitMillicores: 1000,
		MemoryUsedBytes:    256 * 1024 * 1024,
		MemoryLimitBytes:   1024 * 1024 * 1024,
		DiskUsedBytes:      512 * 1024 * 1024,
		DiskLimitBytes:     10 * 1024 * 1024 * 1024,
		ObservedUnix:       time.Now().Unix(),
	}, nil
}

// InitializeSystem simulates init.sh.
func (p *DummyProvisioner) InitializeSystem(ctx context.Context, instanceName string, request BootstrapRequest) (SystemIdentity, error) {
	if err := ctx.Err(); err != nil {
		return SystemIdentity{}, err
	}
	if instanceName == "" {
		return SystemIdentity{}, fmt.Errorf("instance name is empty")
	}
	if request.CodespaceUUID == "" {
		return SystemIdentity{}, fmt.Errorf("codespace uuid is empty")
	}
	return SystemIdentity{UID: 1000, GID: 1000, SharedEnv: map[string]string{}}, nil
}

// PrepareWorkspace simulates start.sh/resume.sh prepare.
func (p *DummyProvisioner) PrepareWorkspace(ctx context.Context, instanceName string, request BootstrapRequest) (WorkspaceStatus, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceStatus{}, err
	}
	if instanceName == "" {
		return WorkspaceStatus{}, fmt.Errorf("instance name is empty")
	}
	if request.CodespaceUUID == "" {
		return WorkspaceStatus{}, fmt.Errorf("codespace uuid is empty")
	}
	if request.Workdir == "" {
		return WorkspaceStatus{}, fmt.Errorf("workdir is empty")
	}
	return WorkspaceStatus{Workdir: request.Workdir, SharedEnv: map[string]string{}}, nil
}

// ActivateRuntime simulates start.sh/resume.sh activate.
func (p *DummyProvisioner) ActivateRuntime(ctx context.Context, instanceName string, request BootstrapRequest) (RuntimeAccess, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeAccess{}, err
	}
	if instanceName == "" {
		return RuntimeAccess{}, fmt.Errorf("instance name is empty")
	}
	if request.CodespaceUUID == "" {
		return RuntimeAccess{}, fmt.Errorf("codespace uuid is empty")
	}
	return RuntimeAccess{
		SharedEnv: map[string]string{},
	}, nil
}

// Stop marks one instance as stopped.
func (p *DummyProvisioner) Stop(ctx context.Context, instanceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.instances[instanceName]; !ok {
		return nil
	}
	p.instances[instanceName].RuntimeState = RuntimeStateStopped
	return nil
}

// Delete deletes one instance.
func (p *DummyProvisioner) Delete(ctx context.Context, instanceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.instances, instanceName)
	return nil
}

type dummyWorkspaceCommandSession struct {
	stdin    *io.PipeWriter
	stdout   *io.PipeReader
	stderr   *io.PipeReader
	waitDone chan error
}

func (s *dummyWorkspaceCommandSession) Stdin() io.WriteCloser {
	return s.stdin
}

func (s *dummyWorkspaceCommandSession) Stdout() io.Reader {
	return s.stdout
}

func (s *dummyWorkspaceCommandSession) Stderr() io.Reader {
	return s.stderr
}

func (s *dummyWorkspaceCommandSession) Resize(int, int) error {
	return nil
}

func (s *dummyWorkspaceCommandSession) Wait() error {
	return <-s.waitDone
}

func (s *dummyWorkspaceCommandSession) Close() error {
	_ = s.stdin.Close()
	return nil
}
