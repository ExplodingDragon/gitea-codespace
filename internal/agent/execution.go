// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/certificates"
	"gitea.dev/codespace/internal/devcontainerruntime"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"google.golang.org/protobuf/proto"
)

const (
	runtimeVolume         = "/var/lib/codespace"
	runtimeRunDirectory   = "/run/codespace"
	runtimeGiteaTokenPath = runtimeRunDirectory + "/gitea-token"
	runtimeGitDirectory   = runtimeRunDirectory + "/git"
	workspaceRoot         = runtimeVolume + "/workspaces"
)

var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// Runtime executes one Gitea lifecycle operation against the Pod-local Docker
// daemon. Persistent facts are owned by Journal; credentials stay in tmpfs.
type Runtime struct {
	Journal                    *Journal
	PodUID                     string
	DockerDirectory            string
	IdentityDirectory          string
	DockerCertificateDirectory string
	Diagnostics                io.Writer
	DevContainer               devcontainerruntime.Configuration
	systemCABundlePath         string
	dockerCABundlePath         string

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

func (r *Runtime) Execute(ctx context.Context, response *agentv1.ControlResponse, update func(*agentv1.AgentReport), stdout, stderr io.Writer) (returnErr error) {
	defer func() {
		if ctx.Err() != nil {
			r.mu.Lock()
			r.target = nil
			r.mu.Unlock()
			// Closing attach does not terminate Docker exec; stop this writer before reporting cancellation.
			returnErr = errors.Join(returnErr, ctx.Err(), r.Close())
		}
	}()
	if r.Journal == nil || r.PodUID == "" || response == nil || response.Operation == nil || response.Runtime == nil {
		return fmt.Errorf("runtime execution input is incomplete")
	}
	cacheRootCA, err := r.ensureDocker(ctx, response.Runtime.Cache)
	if err != nil {
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
		return r.create(ctx, executor, response, cacheRootCA, report, stdout, stderr)
	case operation.GetResume() != nil:
		return r.resume(ctx, executor, response, report)
	case operation.GetStop() != nil:
		_, err := executor.Apply(ctx, devcontainerruntime.Request{Action: "stop", CodespaceUUID: operation.RuntimeUuid, OperationVersion: operation.OperationRversion})
		return err
	case operation.GetDelete() != nil:
		// Kubernetes removes the Pod before deleting its PVC. If the control
		// message arrives first, stopping Docker resources shortens shutdown.
		_, err := executor.Apply(ctx, devcontainerruntime.Request{Action: "stop", CodespaceUUID: operation.RuntimeUuid, OperationVersion: operation.OperationRversion})
		return err
	default:
		return fmt.Errorf("runtime operation is not executable by the Agent")
	}
}

func (r *Runtime) ensureDocker(ctx context.Context, cache *agentv1.RuntimeCache) ([]byte, error) {
	r.dockerMu.Lock()
	defer r.dockerMu.Unlock()
	root, err := r.prepareDockerCacheTrust(cache)
	if err != nil {
		return nil, err
	}
	if r.docker != nil {
		return root, nil
	}
	directory := r.DockerDirectory
	if directory == "" {
		directory = runtimeVolume
	}
	daemon, err := StartDocker(ctx, directory, r.Diagnostics)
	if err != nil {
		return nil, err
	}
	r.docker = daemon
	return root, nil
}

func (r *Runtime) prepareDockerCacheTrust(cache *agentv1.RuntimeCache) ([]byte, error) {
	registries := map[string]struct{}{}
	if cache != nil {
		values := []string{cache.BuildRegistry}
		for _, mirror := range cache.Mirrors {
			values = append(values, mirror)
		}
		for _, value := range values {
			parsed, err := url.Parse(value)
			if err != nil {
				return nil, fmt.Errorf("parse runtime cache registry: %w", err)
			}
			if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
				return nil, fmt.Errorf("runtime cache registry must be an absolute HTTPS URL without credentials, query, or fragment")
			}
			registries[parsed.Host] = struct{}{}
		}
	}
	var internalRoot []byte
	if len(registries) > 0 {
		identityDirectory := r.IdentityDirectory
		if identityDirectory == "" {
			identityDirectory = "/run/identity"
		}
		root, err := os.ReadFile(filepath.Join(identityDirectory, "ca.crt"))
		if err != nil {
			return nil, fmt.Errorf("read internal cache CA: %w", err)
		}
		if parsed, err := certificates.ParseCABundle(string(root)); err != nil || len(parsed) != 1 {
			return nil, fmt.Errorf("internal cache CA is invalid")
		}
		internalRoot = root
		certificateDirectory := r.DockerCertificateDirectory
		if certificateDirectory == "" {
			certificateDirectory = "/etc/docker/certs.d"
		}
		for registry := range registries {
			directory := filepath.Join(certificateDirectory, registry)
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return nil, fmt.Errorf("create Docker registry trust directory: %w", err)
			}
			if err := replaceCAFile(filepath.Join(directory, "ca.crt"), root); err != nil {
				return nil, fmt.Errorf("install Docker registry CA: %w", err)
			}
		}
	}
	var proxyRoot []byte
	if path := strings.TrimSpace(os.Getenv("CODESPACE_PROXY_CA_FILE")); path != "" {
		var err error
		proxyRoot, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read proxy CA bundle: %w", err)
		}
		if _, err := certificates.ParseCABundle(string(proxyRoot)); err != nil {
			return nil, fmt.Errorf("proxy CA bundle: %w", err)
		}
	}
	if len(internalRoot) == 0 && len(proxyRoot) == 0 {
		return nil, nil
	}
	systemBundle := r.systemCABundlePath
	if systemBundle == "" {
		systemBundle = "/etc/ssl/certs/ca-certificates.crt"
	}
	systemRoots, err := os.ReadFile(systemBundle)
	if err != nil {
		return nil, fmt.Errorf("read system CA bundle: %w", err)
	}
	dockerBundle := r.dockerCABundlePath
	if dockerBundle == "" {
		dockerBundle = runtimeRunDirectory + "/docker-ca-bundle.pem"
	}
	combined := append([]byte(nil), systemRoots...)
	for _, bundle := range [][]byte{internalRoot, proxyRoot} {
		if len(bundle) == 0 {
			continue
		}
		if len(combined) > 0 && combined[len(combined)-1] != '\n' {
			combined = append(combined, '\n')
		}
		combined = append(combined, bundle...)
	}
	if err := os.MkdirAll(filepath.Dir(dockerBundle), 0o700); err != nil {
		return nil, fmt.Errorf("create Docker CA bundle directory: %w", err)
	}
	if err := replaceCAFile(dockerBundle, combined); err != nil {
		return nil, fmt.Errorf("install Docker CA bundle: %w", err)
	}
	if err := os.Setenv("SSL_CERT_FILE", dockerBundle); err != nil {
		return nil, fmt.Errorf("configure Docker CA bundle: %w", err)
	}
	return internalRoot, nil
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

func (r *Runtime) create(ctx context.Context, executor *Executor, response *agentv1.ControlResponse, cacheRootCA []byte, report func(codespacev1.RuntimeBootStage, *agentv1.AccessTarget), stdout, stderr io.Writer) error {
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
	trust, err := prepareSiteTrust(response.Runtime, access.GiteaServerUrl, r.IdentityDirectory)
	if err != nil {
		return err
	}
	secrets, err := r.prepareAccess(access, response.Runtime.GitSshKeyType, hostUser)
	if err != nil {
		return err
	}
	workspace, err := repositoryWorkspace(payload.Repository)
	if err != nil {
		return err
	}
	if err := ensureWorkspace(ctx, operation, payload.Repository, hostUser, trust, stdout, stderr); err != nil {
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
	var cacheCredentials map[string]*agentv1.RegistryCredential
	if value := response.Runtime.Cache; value != nil {
		cache.BuildRegistry, cache.BuildScope = value.BuildRegistry, value.BuildScope
		cache.Mirrors = maps.Clone(value.Mirrors)
		cache.RootCAPEM = append([]byte(nil), cacheRootCA...)
		cacheCredentials = value.Credentials
	}
	cleanupCredentials, err := prepareDockerCredentials(runtimeDockerConfigDirectory, cacheCredentials)
	if err != nil {
		return err
	}
	if len(cacheCredentials) > 0 {
		cache.Credentials = make(map[string]devcontainer.RegistryCredential, len(cacheCredentials))
		for registry, credential := range cacheCredentials {
			cache.Credentials[strings.TrimSpace(registry)] = devcontainer.RegistryCredential{
				Username: strings.TrimSpace(credential.Username),
				Password: credential.Password,
			}
		}
	}
	state, applyErr := executor.Apply(ctx, devcontainerruntime.Request{
		Action: "create", CodespaceUUID: operation.RuntimeUuid,
		OperationVersion: operation.OperationRversion, Workspace: workspace, Source: source, HostUser: hostUser,
		GitUserName: strings.TrimSpace(payload.GitIdentity.GiteaUsername), GitUserEmail: strings.TrimSpace(payload.GitIdentity.GitUserEmail),
		GiteaURL: trust.giteaURL, GiteaCustomCA: trust.customCA, LocalEnvironment: runtimeLocalEnvironment(hostUser), Secrets: secrets, Cache: cache, DevContainer: r.DevContainer,
	})
	cleanupErr := cleanupCredentials()
	if applyErr != nil || cleanupErr != nil {
		return errors.Join(applyErr, cleanupErr)
	}
	endpoints, _, err := devcontainerruntime.ConfiguredEndpoints(state)
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
	trust, err := prepareSiteTrust(response.Runtime, response.Access.GiteaServerUrl, r.IdentityDirectory)
	if err != nil {
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
		Action: "resume", CodespaceUUID: response.Operation.RuntimeUuid,
		OperationVersion: response.Operation.OperationRversion, HostUser: hostUser, GiteaURL: trust.giteaURL, GiteaCustomCA: trust.customCA, LocalEnvironment: runtimeLocalEnvironment(hostUser), Secrets: secrets, DevContainer: r.DevContainer,
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
	_, containers, err := devcontainerruntime.ConfiguredEndpoints(state)
	if err != nil {
		return nil, err
	}
	return accessTargetForContainer(version, state.PrimaryContainerID, r.DevContainer.WebIDE.Enabled, r.endpoints, containers), nil
}

func accessTargetForContainer(version int64, primaryContainerID string, webIDEEnabled bool, endpoints []*codespacev1.RuntimeEndpoint, containers map[string]string) *agentv1.AccessTarget {
	target := &agentv1.AccessTarget{Version: version, PrimaryContainerId: primaryContainerID, Ready: true}
	if webIDEEnabled {
		target.Endpoints = append(target.Endpoints, &agentv1.EndpointTarget{
			Endpoint:    &codespacev1.RuntimeEndpoint{EndpointId: runtimeendpoint.WorkspaceEndpointID, Label: runtimeendpoint.WorkspaceEndpointLabel},
			ContainerId: primaryContainerID, Port: runtimeendpoint.WorkspaceEndpointPort,
		})
	}
	for _, endpoint := range endpoints {
		containerID := containers[endpoint.EndpointId]
		if containerID == "" {
			containerID = primaryContainerID
		}
		target.Endpoints = append(target.Endpoints, &agentv1.EndpointTarget{
			Endpoint: proto.Clone(endpoint).(*codespacev1.RuntimeEndpoint), ContainerId: containerID, Port: endpoint.Port,
		})
	}
	return target
}
