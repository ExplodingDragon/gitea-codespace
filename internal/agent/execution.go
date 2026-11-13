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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/cgroup"
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
	Debug                      bool
	Isolation                  string
	SSHEnvironment             map[string]string
	Cgroup                     cgroup.Reader
	Journal                    *Journal
	PodUID                     string
	DockerDirectory            string
	IdentityDirectory          string
	DockerCertificateDirectory string
	Diagnostics                io.Writer
	DevContainer               devcontainerruntime.Configuration
	NativeDevices              *NativeDevices
	USBIP                      *USBIPRuntime
	forwards                   reverseForwards
	networkCtx                 context.Context
	cancelNetwork              context.CancelFunc
	systemCABundlePath         string
	proxyCABundlePath          string
	outboundCABundlePath       string

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
		if returnErr != nil && r.Debug {
			writeFailureDiagnostics(stderr, r.Cgroup, r.Isolation == "kata")
		}
		if ctx.Err() != nil {
			// A cancelled operation must release its network streams and Docker
			// client, but the Agent-owned Guest mounts stay available for the next
			// operation in this worker.
			returnErr = errors.Join(returnErr, ctx.Err(), r.closeOperation())
		}
	}()
	if r.Journal == nil || r.PodUID == "" || response == nil || response.Operation == nil || response.Runtime == nil {
		return fmt.Errorf("runtime execution input is incomplete")
	}
	_, _ = fmt.Fprintln(stdout, "[INFO] [runtime] Preparing trust and container engine")
	trust, err := r.prepareRuntimeTrust(response.Runtime)
	if err != nil {
		return err
	}
	if err := r.ensureDocker(ctx, response.Runtime.Cache, trust); err != nil {
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
		return r.create(ctx, executor, response, trust, report, stdout, stderr)
	case operation.GetResume() != nil:
		return r.resume(ctx, executor, response, trust, report)
	case operation.GetStop() != nil:
		_, err := executor.Apply(ctx, devcontainerruntime.Request{Debug: r.Debug, Action: "stop", CodespaceUUID: operation.RuntimeUuid, OperationVersion: operation.OperationRversion, DevContainer: r.DevContainer, USBIP: r.usbIPContainerConfiguration(), BeforeContainerStop: r.closeNetworkAccess})
		return err
	case operation.GetDelete() != nil:
		// Kubernetes removes the Pod before deleting its PVC. If the control
		// message arrives first, stopping Docker resources shortens shutdown.
		_, err := executor.Apply(ctx, devcontainerruntime.Request{Debug: r.Debug, Action: "delete", CodespaceUUID: operation.RuntimeUuid, OperationVersion: operation.OperationRversion, USBIP: r.usbIPContainerConfiguration(), BeforeContainerStop: r.closeNetworkAccess})
		return err
	default:
		return fmt.Errorf("runtime operation is not executable by the Agent")
	}
}

func (r *Runtime) ensureDocker(ctx context.Context, cache *agentv1.RuntimeCache, trust runtimeTrust) error {
	r.dockerMu.Lock()
	defer r.dockerMu.Unlock()
	registries, err := dockerRegistryHosts(cache, trust.giteaURL)
	if err != nil {
		return err
	}
	certificateDirectory := r.DockerCertificateDirectory
	if certificateDirectory == "" {
		certificateDirectory = "/etc/docker/certs.d"
	}
	for registry := range registries {
		directory := filepath.Join(certificateDirectory, registry)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create Docker registry trust directory: %w", err)
		}
		if err := replaceCAFile(filepath.Join(directory, "ca.crt"), trust.caBundle); err != nil {
			return fmt.Errorf("install Docker registry CA bundle: %w", err)
		}
	}
	if r.docker != nil {
		return nil
	}
	directory := r.DockerDirectory
	if directory == "" {
		directory = runtimeVolume
	}
	cdiSpecDirectory := ""
	if r.NativeDevices != nil {
		cdiSpecDirectory = r.NativeDevices.CDISpecDirectory
	}
	daemon, err := StartDocker(ctx, directory, cdiSpecDirectory, r.Isolation, r.Cgroup, trust.caBundlePath, r.Diagnostics)
	if err != nil {
		return err
	}
	r.docker = daemon
	return nil
}

func (r *Runtime) closeOperation() error {
	r.closeNetworkAccess()
	r.dockerMu.Lock()
	docker := r.docker
	r.docker = nil
	r.dockerMu.Unlock()
	if docker == nil {
		return nil
	}
	return docker.Close()
}

// Close stops the Runtime-owned Docker client and releases Agent-owned USB/IP
// mounts before the worker exits.
func (r *Runtime) Close() error {
	operationErr := r.closeOperation()
	if r.USBIP == nil {
		return operationErr
	}
	return errors.Join(operationErr, r.USBIP.Close())
}

func (r *Runtime) create(ctx context.Context, executor *Executor, response *agentv1.ControlResponse, trust runtimeTrust, report func(codespacev1.RuntimeBootStage, *agentv1.AccessTarget), stdout, stderr io.Writer) error {
	operation, payload, access := response.Operation, response.Operation.GetCreate(), response.Access
	if access == nil || payload.Repository == nil || payload.GitIdentity == nil || payload.DevContainer == nil {
		return fmt.Errorf("create input is incomplete")
	}
	if payload.Repository.RepositoryId <= 0 || payload.GitIdentity.UserId <= 0 || !commitPattern.MatchString(payload.Repository.CommitSha) {
		return fmt.Errorf("create repository identity is invalid")
	}
	username := runtimeUserName(payload.GitIdentity.GiteaUsername)
	_, _ = fmt.Fprintln(stdout, "[INFO] [runtime] Preparing runtime user and repository access")
	hostUser, err := ensureRuntimeUser(username, nil)
	if err != nil {
		return err
	}
	if err := r.saveHostUser(hostUser); err != nil {
		return err
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_INITIALIZE_SYSTEM, nil)
	if err := trust.verifyAccessURL(access.GiteaServerUrl); err != nil {
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
	_, _ = fmt.Fprintln(stdout, "[INFO] [repository] Preparing repository workspace")
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
	cache := devcontainer.CacheOptions{CABundlePEM: append([]byte(nil), trust.caBundle...)}
	var cacheCredentials map[string]*agentv1.RegistryCredential
	if value := response.Runtime.Cache; value != nil {
		cache.BuildRegistry, cache.BuildScope = value.BuildRegistry, value.BuildScope
		cache.Mirrors = maps.Clone(value.Mirrors)
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
	outcome, applyErr := executor.Apply(ctx, devcontainerruntime.Request{
		Debug: r.Debug, HostCgroup: &r.Cgroup,
		Action: "create", CodespaceUUID: operation.RuntimeUuid,
		OperationVersion: operation.OperationRversion, Workspace: workspace, Source: source, HostUser: hostUser,
		GitUserName: strings.TrimSpace(payload.GitIdentity.GiteaUsername), GitUserEmail: strings.TrimSpace(payload.GitIdentity.GitUserEmail),
		GiteaURL: trust.giteaURL, LocalEnvironment: runtimeLocalEnvironment(hostUser), Secrets: secrets, Cache: cache, DevContainer: r.DevContainer, Native: r.nativeContainerConfiguration(), USBIP: r.usbIPContainerConfiguration(),
	})
	cleanupErr := cleanupCredentials()
	if applyErr != nil || cleanupErr != nil {
		return errors.Join(applyErr, cleanupErr)
	}
	state := outcome.Environment
	endpoints, _, err := devcontainerruntime.ConfiguredEndpoints(state)
	if err == nil {
		err = r.replaceConfiguredEndpoints(endpoints)
	}
	if err != nil {
		return err
	}
	return r.publishAccess(operation.OperationRversion, outcome, secrets, report, stdout)
}

func (r *Runtime) resume(ctx context.Context, executor *Executor, response *agentv1.ControlResponse, trust runtimeTrust, report func(codespacev1.RuntimeBootStage, *agentv1.AccessTarget)) error {
	if response.Access == nil {
		return fmt.Errorf("resume access material is missing")
	}
	_, _ = fmt.Fprintln(executor.Stdout, "[INFO] [runtime] Restoring runtime user and repository access")
	hostUser, err := r.loadHostUser()
	if err != nil {
		return err
	}
	if _, err := ensureRuntimeUser(hostUser.Name, &hostUser); err != nil {
		return err
	}
	if err := trust.verifyAccessURL(response.Access.GiteaServerUrl); err != nil {
		return err
	}
	secrets, err := r.prepareAccess(response.Access, response.Runtime.GitSshKeyType, hostUser)
	if err != nil {
		return err
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_INITIALIZE_SYSTEM, nil)
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_WORKSPACE, nil)
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_START_ENVIRONMENT, nil)
	outcome, err := executor.Apply(ctx, devcontainerruntime.Request{
		Debug:  r.Debug,
		Action: "resume", CodespaceUUID: response.Operation.RuntimeUuid,
		OperationVersion: response.Operation.OperationRversion, HostUser: hostUser, GiteaURL: trust.giteaURL, LocalEnvironment: runtimeLocalEnvironment(hostUser), Secrets: secrets, DevContainer: r.DevContainer, Native: r.nativeContainerConfiguration(), USBIP: r.usbIPContainerConfiguration(),
	})
	if err != nil {
		return err
	}
	return r.publishAccess(response.Operation.OperationRversion, outcome, secrets, report, executor.Stdout)
}

func (r *Runtime) publishAccess(version int64, outcome *devcontainerruntime.Outcome, secrets map[string]string, report func(codespacev1.RuntimeBootStage, *agentv1.AccessTarget), stdout io.Writer) error {
	_, _ = fmt.Fprintln(stdout, "[INFO] [runtime] Publishing workspace access")
	target, err := r.accessTarget(version, outcome.Environment, outcome.WebIDEStarted)
	if err != nil {
		return err
	}
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PUBLISH_READY, target)
	r.setAccessState(target, secrets)
	report(codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_READY, target)
	return nil
}

func (r *Runtime) nativeContainerConfiguration() devcontainerruntime.NativeContainerConfiguration {
	if r.NativeDevices == nil {
		return devcontainerruntime.NativeContainerConfiguration{}
	}
	return r.NativeDevices.DevContainer
}

func (r *Runtime) usbIPContainerConfiguration() devcontainerruntime.USBIPContainerConfiguration {
	if r.USBIP == nil || !r.USBIP.Enabled() {
		return devcontainerruntime.USBIPContainerConfiguration{}
	}
	return devcontainerruntime.USBIPContainerConfiguration{Enabled: true, DeviceRoot: USBIPDeviceRootPath, DevptsRoot: USBIPDevptsPath}
}

func (r *Runtime) closeNetworkAccess() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.target = nil
	if r.cancelNetwork != nil {
		r.cancelNetwork()
		r.networkCtx, r.cancelNetwork = nil, nil
	}
	r.forwards.close()
}

func (r *Runtime) setAccessState(target *agentv1.AccessTarget, secrets map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.target != nil && (r.target.PrimaryContainerId != target.PrimaryContainerId || r.target.Version != target.Version) {
		if r.cancelNetwork != nil {
			r.cancelNetwork()
			r.networkCtx, r.cancelNetwork = nil, nil
		}
		r.forwards.close()
	}
	r.target = proto.Clone(target).(*agentv1.AccessTarget)
	r.secrets = maps.Clone(secrets)
}

func (r *Runtime) accessTarget(version int64, state *devcontainer.State, publishWebIDEEndpoint bool) (*agentv1.AccessTarget, error) {
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
	return accessTargetForContainer(version, state.PrimaryContainerID, state.WorkspaceFolder, publishWebIDEEndpoint, r.endpoints, containers), nil
}

func accessTargetForContainer(version int64, primaryContainerID, workspaceFolder string, publishWebIDEEndpoint bool, endpoints []*codespacev1.RuntimeEndpoint, containers map[string]string) *agentv1.AccessTarget {
	target := &agentv1.AccessTarget{Version: version, PrimaryContainerId: primaryContainerID, WorkspaceFolder: workspaceFolder, Ready: true}
	if publishWebIDEEndpoint {
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
