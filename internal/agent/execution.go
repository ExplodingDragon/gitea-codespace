// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"maps"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/devcontainerruntime"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"google.golang.org/protobuf/proto"
)

const (
	runtimeVolume       = "/var/lib/codespace"
	runtimeRunDirectory = "/run/codespace"
	workspaceRoot       = runtimeVolume + "/workspaces"
)

var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// Runtime executes one Gitea lifecycle operation against the Pod-local Docker
// daemon. Persistent facts are owned by Journal; credentials stay in tmpfs.
type Runtime struct {
	Journal         *Journal
	PodUID          string
	DockerDirectory string
	Diagnostics     io.Writer
	DevContainer    devcontainerruntime.Configuration

	dockerMu        sync.Mutex
	mu              sync.RWMutex
	target          *agentv1.AccessTarget
	endpoints       []*codespacev1.RuntimeEndpoint
	endpointsLoaded bool
	targetChanged   chan struct{}
	secrets         map[string]string
	verificationKey []byte
	docker          *DockerDaemon
}

func (r *Runtime) Execute(ctx context.Context, response *agentv1.ControlResponse, update func(*agentv1.AgentReport), stdout, stderr io.Writer) error {
	if r.Journal == nil || r.PodUID == "" || response == nil || response.Operation == nil || response.Runtime == nil {
		return fmt.Errorf("runtime execution input is incomplete")
	}
	if err := r.ensureDocker(ctx, response.Runtime.Cache); err != nil {
		return err
	}
	operation := response.Operation
	if len(response.AccessVerificationKey) > 0 {
		if len(response.AccessVerificationKey) != ed25519.PublicKeySize {
			return fmt.Errorf("manager access verification key is invalid")
		}
		r.mu.Lock()
		r.verificationKey = append(r.verificationKey[:0], response.AccessVerificationKey...)
		r.mu.Unlock()
	}
	started, err := r.Journal.BeginOperation(operation.OperationRversion, time.Now().Unix())
	if err != nil {
		return err
	}
	report := func(stage codespacev1.RuntimeBootStage, target *agentv1.AccessTarget) {
		if update == nil {
			return
		}
		update(&agentv1.AgentReport{
			OperationRversion: operation.OperationRversion,
			Boot: &codespacev1.RuntimeBoot{
				OperationRversion: operation.OperationRversion,
				Stage:             stage, StartedUnix: started, LastUpdateUnix: time.Now().Unix(),
			},
			Target: target,
		})
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_RUNTIME, nil)

	executor := &Executor{Journal: r.Journal, PodUID: r.PodUID, Stdout: stdout, Stderr: stderr}
	switch {
	case operation.GetCreate() != nil:
		return r.create(ctx, executor, response, report, stdout, stderr)
	case operation.GetResume() != nil:
		return r.resume(ctx, executor, response, report)
	case operation.GetStop() != nil:
		_, err := executor.Apply(ctx, devcontainerruntime.Request{Version: devcontainerruntime.FormatVersion, Action: "stop", CodespaceUUID: operation.RuntimeUuid, OperationVersion: operation.OperationRversion})
		return err
	case operation.GetDelete() != nil:
		// Kubernetes removes the Pod before deleting its PVC. If the control
		// message arrives first, stopping Docker resources shortens shutdown.
		_, err := executor.Apply(ctx, devcontainerruntime.Request{Version: devcontainerruntime.FormatVersion, Action: "stop", CodespaceUUID: operation.RuntimeUuid, OperationVersion: operation.OperationRversion})
		return err
	default:
		return fmt.Errorf("runtime operation is not executable by the Agent")
	}
}

func (r *Runtime) ensureDocker(ctx context.Context, cache *agentv1.RuntimeCache) error {
	r.dockerMu.Lock()
	defer r.dockerMu.Unlock()
	if r.docker != nil {
		return nil
	}
	registries := []string{}
	if cache != nil {
		values := []string{cache.BuildRegistry}
		for _, mirror := range cache.Mirrors {
			values = append(values, mirror)
		}
		for _, value := range values {
			parsed, err := url.Parse(value)
			if err != nil {
				return fmt.Errorf("parse runtime cache registry: %w", err)
			}
			if parsed.Scheme == "http" {
				parsed.Path, parsed.RawPath = "", ""
				registries = append(registries, parsed.String())
			}
		}
	}
	directory := r.DockerDirectory
	if directory == "" {
		directory = runtimeVolume
	}
	daemon, err := StartDocker(ctx, directory, registries, r.Diagnostics)
	if err != nil {
		return err
	}
	r.docker = daemon
	return nil
}

// Close stops Docker before the caller releases the Runtime volume lock.
func (r *Runtime) Close() error {
	r.dockerMu.Lock()
	docker := r.docker
	r.docker = nil
	r.dockerMu.Unlock()
	if docker == nil {
		return nil
	}
	return docker.Close()
}

func (r *Runtime) create(ctx context.Context, executor *Executor, response *agentv1.ControlResponse, report func(codespacev1.RuntimeBootStage, *agentv1.AccessTarget), stdout, stderr io.Writer) error {
	operation, payload, access := response.Operation, response.Operation.GetCreate(), response.Access
	if access == nil || payload.Repository == nil || payload.GitIdentity == nil || payload.DevContainer == nil {
		return fmt.Errorf("create input is incomplete")
	}
	if payload.Repository.RepositoryId <= 0 || payload.GitIdentity.UserId <= 0 || !commitPattern.MatchString(payload.Repository.CommitSha) {
		return fmt.Errorf("create repository identity is invalid")
	}
	username := runtimeUserName(payload.GitIdentity.GiteaUsername)
	hostUser, err := ensureRuntimeUser(username, nil)
	if err != nil {
		return err
	}
	if err := r.saveHostUser(hostUser); err != nil {
		return err
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_INITIALIZE_SYSTEM, nil)
	secrets, err := r.prepareAccess(access, response.Runtime.GitSshKeyType, hostUser)
	if err != nil {
		return err
	}
	workspace, err := repositoryWorkspace(payload.Repository)
	if err != nil {
		return err
	}
	err = r.Journal.RunStage(ctx, operation.OperationRversion, "prepare-workspace", func(ctx context.Context) error {
		return cloneRepository(ctx, operation, payload.Repository, hostUser, stdout, stderr)
	})
	if err == nil {
		err = verifyWorkspace(ctx, hostUser, workspace, payload.Repository.CommitSha)
	}
	if err != nil {
		return err
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_WORKSPACE, nil)
	source := devcontainer.Source{}
	switch value := payload.DevContainer.Source.(type) {
	case *codespacev1.DevContainerConfiguration_RepositoryPath:
		source.Path = strings.TrimSpace(value.RepositoryPath)
	case *codespacev1.DevContainerConfiguration_TemplateContent:
		source.Content = strings.TrimSpace(value.TemplateContent)
	default:
		return fmt.Errorf("create Dev Container source is invalid")
	}
	if (source.Path == "") == (source.Content == "") {
		return fmt.Errorf("create Dev Container source must select one value")
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_START_ENVIRONMENT, nil)
	cache := devcontainer.CacheOptions{}
	if value := response.Runtime.Cache; value != nil {
		cache.BuildRegistry, cache.BuildScope = value.BuildRegistry, value.BuildScope
		cache.Mirrors = maps.Clone(value.Mirrors)
		cache.Credentials = make(map[string]devcontainer.RegistryCredential, len(value.Credentials))
		for registry, credential := range value.Credentials {
			if credential == nil || credential.Username == "" || credential.Password == "" {
				return fmt.Errorf("runtime cache credential is invalid")
			}
			cache.Credentials[registry] = devcontainer.RegistryCredential{Username: credential.Username, Password: credential.Password}
		}
	}
	state, err := executor.Apply(ctx, devcontainerruntime.Request{
		Version: devcontainerruntime.FormatVersion, Action: "create", CodespaceUUID: operation.RuntimeUuid,
		OperationVersion: operation.OperationRversion, Workspace: workspace, Source: source, HostUser: hostUser,
		GitUserName: strings.TrimSpace(payload.GitIdentity.GiteaUsername), GitUserEmail: strings.TrimSpace(payload.GitIdentity.GitUserEmail),
		Secrets: secrets, Cache: cache, DevContainer: r.DevContainer,
	})
	if err != nil {
		return err
	}
	endpoints, err := devcontainerruntime.ConfiguredEndpoints(state.Configuration)
	if err == nil {
		err = r.replaceConfiguredEndpoints(endpoints)
	}
	if err != nil {
		return err
	}
	target, err := r.accessTarget(operation.OperationRversion, state)
	if err != nil {
		return err
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PUBLISH_READY, target)
	r.setAccessState(target, secrets)
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY, target)
	return nil
}

func (r *Runtime) resume(ctx context.Context, executor *Executor, response *agentv1.ControlResponse, report func(codespacev1.RuntimeBootStage, *agentv1.AccessTarget)) error {
	if response.Access == nil {
		return fmt.Errorf("resume access material is missing")
	}
	hostUser, err := r.loadHostUser()
	if err != nil {
		return err
	}
	if _, err := ensureRuntimeUser(hostUser.Name, &hostUser); err != nil {
		return err
	}
	secrets, err := r.prepareAccess(response.Access, response.Runtime.GitSshKeyType, hostUser)
	if err != nil {
		return err
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_INITIALIZE_SYSTEM, nil)
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_WORKSPACE, nil)
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_START_ENVIRONMENT, nil)
	state, err := executor.Apply(ctx, devcontainerruntime.Request{
		Version: devcontainerruntime.FormatVersion, Action: "resume", CodespaceUUID: response.Operation.RuntimeUuid,
		OperationVersion: response.Operation.OperationRversion, HostUser: hostUser, Secrets: secrets, DevContainer: r.DevContainer,
	})
	if err != nil {
		return err
	}
	target, err := r.accessTarget(response.Operation.OperationRversion, state)
	if err != nil {
		return err
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PUBLISH_READY, target)
	r.setAccessState(target, secrets)
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY, target)
	return nil
}

func (r *Runtime) setAccessState(target *agentv1.AccessTarget, secrets map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.target = proto.Clone(target).(*agentv1.AccessTarget)
	r.secrets = maps.Clone(secrets)
}

func (r *Runtime) accessTarget(version int64, state *devcontainer.State) (*agentv1.AccessTarget, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadEndpointsLocked(); err != nil {
		return nil, err
	}
	return accessTargetForContainer(version, state.PrimaryContainerID, r.endpoints), nil
}

func accessTargetForContainer(version int64, primaryContainerID string, endpoints []*codespacev1.RuntimeEndpoint) *agentv1.AccessTarget {
	target := &agentv1.AccessTarget{Version: version, PrimaryContainerId: primaryContainerID, Ready: true}
	target.Endpoints = append(target.Endpoints, &agentv1.EndpointTarget{
		Endpoint:    &codespacev1.RuntimeEndpoint{EndpointId: runtimeendpoint.WorkspaceEndpointID, Label: runtimeendpoint.WorkspaceEndpointLabel},
		ContainerId: primaryContainerID, Port: runtimeendpoint.WorkspaceEndpointPort,
	})
	for _, endpoint := range endpoints {
		target.Endpoints = append(target.Endpoints, &agentv1.EndpointTarget{
			Endpoint: proto.Clone(endpoint).(*codespacev1.RuntimeEndpoint), ContainerId: primaryContainerID, Port: endpoint.Port,
		})
	}
	return target
}
