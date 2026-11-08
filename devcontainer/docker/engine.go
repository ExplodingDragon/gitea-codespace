// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	containercommand "github.com/docker/cli/cli/command/container"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	compose "github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"gitea.dev/codespace/devcontainer"
)

const (
	labelOwnerID       = "devcontainer.owner_id"
	labelEnvironmentID = "devcontainer.environment_id"
	labelMetadata      = "devcontainer.metadata"
)

// Engine creates and controls one complete Dev Container environment.
type Engine struct {
	cli      *command.DockerCli
	client   client.APIClient
	compose  api.Compose
	stdout   io.Writer
	stderr   io.Writer
	platform *ocispec.Platform
}

// PrepareLifecycleFunc prepares caller-specific state after the remote user and
// environment are known and before the first in-container lifecycle command.
type PrepareLifecycleFunc func(context.Context, *Engine, *devcontainer.State) error

// ImageCustomization describes the final image layer requested by a caller.
// The callback writes a Dockerfile and any required assets into ContextDir.
type ImageCustomization struct {
	ContextDir    string
	BaseImage     string
	ContainerUser string
	Platform      ocispec.Platform
}

// CustomizeImageFunc writes caller-owned image contents into a temporary
// Docker build context managed by the Engine.
type CustomizeImageFunc func(ImageCustomization) error

// ReadyFunc reports the first lifecycle stage selected by waitFor.
type ReadyFunc func(*devcontainer.State, devcontainer.LifecycleStage)

// RunLifecycleFunc durably executes one lifecycle stage. Callers may leave it
// nil when no external journal is used.
type RunLifecycleFunc func(context.Context, devcontainer.LifecycleStage, func(context.Context) error) error

// CreateOptions contains caller identity, configuration input, and integration hooks.
type CreateOptions struct {
	OwnerID             string
	Workspace           string
	Source              devcontainer.Source
	AllowedPathRoot     string
	HostUser            devcontainer.HostUser
	LocalEnvironment    map[string]string
	PlatformEnvironment map[string]string
	Secrets             map[string]string
	InjectedFeatures    []devcontainer.InjectedFeature
	AdditionalMounts    []devcontainer.Mount
	ContainerEnv        map[string]string
	Labels              map[string]string
	PlatformDevices     PlatformDevices
	FrozenLockfile      bool
	Cache               devcontainer.CacheOptions
	CustomizeImage      CustomizeImageFunc
	PrepareLifecycle    PrepareLifecycleFunc
	RunLifecycle        RunLifecycleFunc
	Ready               ReadyFunc
}

// PlatformDevices contains the device access selected by the environment
// template. It is applied only to the primary Dev Container.
type PlatformDevices struct {
	CDI          []string
	Mappings     []DeviceMapping
	Capabilities []string
	ProvidesGPU  bool
}

type DeviceMapping struct {
	Source      string
	Target      string
	Permissions string
}

// New connects an Engine to the Docker endpoint selected by the standard Docker client configuration.
func New(ctx context.Context, stdout, stderr io.Writer) (*Engine, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cli, err := command.NewDockerCli(
		command.WithBaseContext(ctx),
		command.WithInputStream(io.NopCloser(strings.NewReader(""))),
		command.WithOutputStream(stdout),
		command.WithErrorStream(stderr),
	)
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	if err := cli.Initialize(flags.NewClientOptions()); err != nil {
		return nil, fmt.Errorf("initialize Docker client: %w", err)
	}
	dockerClient := cli.Client()
	version, err := dockerClient.ServerVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect Docker server: %w", err)
	}
	if strings.TrimSpace(version.Os) == "" || strings.TrimSpace(version.Arch) == "" {
		return nil, fmt.Errorf("docker server platform is incomplete")
	}
	return &Engine{
		cli:      cli,
		client:   dockerClient,
		compose:  compose.NewComposeService(cli, compose.WithPrompt(func(string, bool) (bool, error) { return true, nil })),
		stdout:   stdout,
		stderr:   stderr,
		platform: &ocispec.Platform{OS: version.Os, Architecture: version.Arch},
	}, nil
}

// Close releases the Docker client connection.
func (e *Engine) Close() error {
	return e.client.Close()
}

// Create resolves configuration, creates resources, and completes the first lifecycle.
func (e *Engine) Create(ctx context.Context, options CreateOptions) (*devcontainer.State, error) {
	if strings.TrimSpace(options.OwnerID) == "" {
		return nil, fmt.Errorf("dev container owner ID is empty")
	}
	environmentID := devContainerID(options.OwnerID)
	resolved, err := devcontainer.Load(devcontainer.LoadOptions{
		Workspace:       options.Workspace,
		Source:          options.Source,
		LocalEnv:        options.LocalEnvironment,
		ID:              environmentID,
		AllowedPathRoot: options.AllowedPathRoot,
	})
	if err != nil {
		return nil, err
	}
	if err := validateCacheOptions(options.Cache); err != nil {
		return nil, err
	}
	composeProject := ""
	if len(resolved.DockerComposeFile) > 0 {
		composeProject = composeProjectName(options.OwnerID)
	}
	if err := e.cleanupIncompleteCreate(ctx, options.OwnerID, composeProject); err != nil {
		return nil, fmt.Errorf("clean incomplete Dev Container environment: %w", err)
	}
	if err := mergeInjectedFeatures(resolved, options.InjectedFeatures); err != nil {
		return nil, err
	}
	resolved.FrozenLockfile = options.FrozenLockfile
	resolved.Cache = options.Cache
	resolved.Configuration = devcontainer.Merge(resolved.Configuration, devcontainer.Configuration{Mounts: options.AdditionalMounts, ContainerEnv: options.ContainerEnv})
	if err := checkHostRequirements(resolved.HostRequirements, resolved.Workspace); err != nil {
		return nil, err
	}
	if err := runLifecycleStage(ctx, options.RunLifecycle, devcontainer.LifecycleStageInitialize, resolved.InitializeCommand, func(ctx context.Context) error {
		return runInitializeCommand(ctx, resolved.InitializeCommand, options.HostUser, resolved.Workspace, options.LocalEnvironment, e.stdout, e.stderr)
	}); err != nil {
		return nil, fmt.Errorf("run initializeCommand: %w", err)
	}

	workspaceFolder := resolved.WorkspaceFolder
	if workspaceFolder == "" {
		if len(resolved.DockerComposeFile) > 0 {
			workspaceFolder = "/"
		} else {
			workspaceFolder = path.Join(devcontainer.DefaultWorkspaceFolder, filepath.Base(resolved.Workspace))
		}
	}
	workspaceFolder = path.Clean(workspaceFolder)
	if !devcontainer.IsValidWorkspaceFolder(workspaceFolder) {
		return nil, fmt.Errorf("dev container workspaceFolder must be a canonical absolute container path")
	}
	resolved.WorkspaceFolder = workspaceFolder
	labels := maps.Clone(options.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelOwnerID] = options.OwnerID
	labels[labelEnvironmentID] = environmentID

	var primaryID string
	var related []string
	var featureDigests map[string]string
	var primaryService string
	var composeContainers map[string]string
	if len(resolved.DockerComposeFile) > 0 {
		primaryService = resolved.Service
		primaryID, related, composeContainers, featureDigests, err = e.createCompose(ctx, resolved, composeProject, labels, options.HostUser, options.PlatformEnvironment, options.PlatformDevices, options.CustomizeImage)
	} else {
		primaryID, featureDigests, err = e.createContainer(ctx, resolved, workspaceFolder, labels, options.HostUser, options.PlatformEnvironment, options.PlatformDevices, options.CustomizeImage)
	}
	if err != nil {
		return nil, err
	}
	stateConfiguration := resolved.Configuration
	stateConfiguration.OnCreateCommand = devcontainer.Command{}
	stateConfiguration.UpdateContentCommand = devcontainer.Command{}
	stateConfiguration.PostCreateCommand = devcontainer.Command{}
	stateConfiguration.PostStartCommand = devcontainer.Command{}
	stateConfiguration.PostAttachCommand = devcontainer.Command{}
	environment := &devcontainer.State{
		Version:             devcontainer.StateFormatVersion,
		ID:                  environmentID,
		OwnerID:             options.OwnerID,
		ConfigurationPath:   resolved.ConfigurationPath,
		ConfigurationSHA256: resolved.ContentSHA256,
		Configuration:       stateConfiguration,
		Workspace:           resolved.Workspace,
		WorkspaceFolder:     workspaceFolder,
		ComposeProject:      composeProject,
		PrimaryService:      primaryService,
		ComposeContainers:   composeContainers,
		PrimaryContainerID:  primaryID,
		RelatedContainerIDs: related,
		RemoteUser:          strings.TrimSpace(resolved.RemoteUser),
		RemoteWorkdir:       workspaceFolder,
		RemoteEnvironment:   map[string]string{},
		FeatureDigests:      featureDigests,
		LifecycleCommands:   resolved.LifecycleCommands,
	}
	if err := environment.Validate(); err != nil {
		_ = e.Delete(context.WithoutCancel(ctx), environment)
		return nil, err
	}
	if err := e.initializeContainer(ctx, environment, options.Secrets, options.PrepareLifecycle, options.RunLifecycle, options.Ready); err != nil {
		_ = e.Delete(context.WithoutCancel(ctx), environment)
		return nil, err
	}
	return environment, nil
}

func devContainerID(ownerID string) string {
	digest := sha256.Sum256([]byte(ownerID))
	return hex.EncodeToString(digest[:])
}

func mergeInjectedFeatures(resolved *devcontainer.ResolvedConfiguration, injected []devcontainer.InjectedFeature) error {
	repositoryFeatures := make(map[string]string, len(resolved.Features))
	repositoryFeatureIdentities := make(map[string][]string, len(resolved.Features))
	for reference := range resolved.Features {
		canonicalReference := strings.ToLower(strings.TrimSpace(reference))
		if existing := repositoryFeatures[canonicalReference]; existing != "" {
			return fmt.Errorf("repository Dev Container Features %s and %s differ only by case", existing, reference)
		}
		repositoryFeatures[canonicalReference] = reference
		if identity, err := featureReferenceID(reference); err == nil {
			repositoryFeatureIdentities[identity] = append(repositoryFeatureIdentities[identity], reference)
		}
	}
	resolved.PlatformFeatureReferences = map[string]struct{}{}
	resolved.InstallOnlyFeatures = map[string]struct{}{}
	for _, feature := range injected {
		reference := strings.TrimSpace(feature.Reference)
		canonicalReference := strings.ToLower(reference)
		origin := strings.TrimSpace(feature.Origin)
		if reference == "" || origin == "" {
			return fmt.Errorf("injected Dev Container Feature identity is incomplete")
		}
		featureOptions, err := json.Marshal(feature.Options)
		if err != nil {
			return fmt.Errorf("encode %s Feature %s options: %w", origin, reference, err)
		}
		repositoryReference, repositoryOwned := repositoryFeatures[canonicalReference]
		if identity, parseErr := featureReferenceID(reference); parseErr == nil {
			existing := repositoryFeatureIdentities[identity]
			if len(existing) > 1 || (len(existing) == 1 && !strings.EqualFold(existing[0], reference)) {
				slices.Sort(existing)
				return fmt.Errorf("repository Dev Container Feature %s conflicts with injected Feature %s from %s because they select different versions of the same Feature", strings.Join(existing, ", "), reference, origin)
			}
		}
		if repositoryOwned {
			reference = repositoryReference
		}
		if existing, ok := resolved.Features[reference]; ok {
			featureOptions, err = mergeFeatureOptions(existing, featureOptions)
			if err != nil {
				return fmt.Errorf("merge dev container feature %s from repository and %s: %w", reference, origin, err)
			}
		}
		if resolved.Features == nil {
			resolved.Features = map[string]json.RawMessage{}
		}
		resolved.Features[reference] = featureOptions
		if !repositoryOwned {
			resolved.PlatformFeatureReferences[canonicalReference] = struct{}{}
		}
		if feature.InstallOnly {
			resolved.InstallOnlyFeatures[reference] = struct{}{}
		}
	}
	return nil
}

func mergeFeatureOptions(base, override json.RawMessage) (json.RawMessage, error) {
	decode := func(raw json.RawMessage) (map[string]json.RawMessage, error) {
		enabled, normalized, err := normalizeFeatureRequest(raw)
		if err != nil {
			return nil, err
		}
		if !enabled {
			return nil, fmt.Errorf("feature is disabled")
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(normalized, &result); err != nil {
			return nil, err
		}
		return result, nil
	}
	merged, err := decode(base)
	if err != nil {
		return nil, err
	}
	values, err := decode(override)
	if err != nil {
		return nil, err
	}
	maps.Copy(merged, values)
	return json.Marshal(merged)
}

func (e *Engine) createContainer(ctx context.Context, resolved *devcontainer.ResolvedConfiguration, workspaceFolder string, labels map[string]string, hostUser devcontainer.HostUser, platformEnvironment map[string]string, platformDevices PlatformDevices, customizeImage CustomizeImageFunc) (string, map[string]string, error) {
	imageName, featureDigests, err := e.resolveImage(ctx, resolved, platformEnvironment)
	if err != nil {
		return "", nil, err
	}
	imageName, err = e.prepareUserImage(ctx, imageName, resolved, hostUser)
	if err != nil {
		return "", nil, err
	}
	imageName, err = e.customizeImage(ctx, imageName, resolved, customizeImage, platformEnvironment)
	if err != nil {
		return "", nil, err
	}
	mounts, err := containerMounts(resolved.Workspace, workspaceFolder, resolved.WorkspaceMount, resolved.Mounts)
	if err != nil {
		return "", nil, err
	}
	publishSpecs, err := appPortPublishSpecs(resolved.AppPort)
	if err != nil {
		return "", nil, err
	}
	exposedPorts, portBindings, err := nat.ParsePortSpecs(publishSpecs)
	if err != nil {
		return "", nil, fmt.Errorf("appPort: %w", err)
	}
	useGPU, err := e.resolveGPURequest(ctx, resolved.HostRequirements.GPU, platformDevices.ProvidesGPU)
	if err != nil {
		return "", nil, err
	}
	if platformDevices.ProvidesGPU {
		resolved.RunArgs, err = platformGPUArgs(resolved.RunArgs)
		if err != nil {
			return "", nil, err
		}
	}
	override := resolved.OverrideCommand == nil || *resolved.OverrideCommand
	config := &container.Config{
		Image:      imageName,
		Env:        devcontainer.EnvironmentList(mergePlatformEnvironment(resolved.ContainerEnv, platformEnvironment)),
		Labels:     labels,
		Tty:        true,
		OpenStdin:  true,
		StdinOnce:  false,
		WorkingDir: workspaceFolder,
		User:       resolved.ContainerUser,
	}
	if override || len(resolved.FeatureEntrypoints) > 0 {
		var arguments []string
		if !override {
			inspect, err := e.inspectImage(ctx, imageName)
			if err != nil {
				return "", nil, fmt.Errorf("inspect Dev Container command: %w", err)
			}
			if inspect.Config != nil {
				arguments = append(arguments, inspect.Config.Entrypoint...)
				arguments = append(arguments, inspect.Config.Cmd...)
			}
		}
		config.Entrypoint = []string{"/bin/sh"}
		config.Cmd = append([]string{"-c", containerStartupScript(resolved.FeatureEntrypoints), "-"}, arguments...)
	}
	hostConfig := &container.HostConfig{
		Mounts:       mounts,
		Privileged:   resolved.Privileged,
		CapAdd:       slices.Clone(resolved.CapAdd),
		SecurityOpt:  slices.Clone(resolved.SecurityOpt),
		Init:         &resolved.Init,
		PortBindings: portBindings,
	}
	hostConfig.CapAdd = append(hostConfig.CapAdd, platformDevices.Capabilities...)
	for _, mapping := range platformDevices.Mappings {
		hostConfig.Devices = append(hostConfig.Devices, container.DeviceMapping{PathOnHost: mapping.Source, PathInContainer: mapping.Target, CgroupPermissions: mapping.Permissions})
	}
	if len(platformDevices.CDI) > 0 {
		hostConfig.DeviceRequests = append(hostConfig.DeviceRequests, container.DeviceRequest{Driver: "cdi", DeviceIDs: slices.Clone(platformDevices.CDI)})
	}
	config.ExposedPorts = exposedPorts
	if useGPU {
		hostConfig.DeviceRequests = append(hostConfig.DeviceRequests, container.DeviceRequest{Count: -1, Capabilities: [][]string{{"gpu"}}})
	}
	if len(resolved.RunArgs) > 0 {
		containerID, err := e.createContainerWithRunArgs(ctx, resolved, config, hostConfig, publishSpecs)
		return containerID, featureDigests, err
	}
	response, err := e.client.ContainerCreate(ctx, config, hostConfig, &network.NetworkingConfig{}, nil, "devcontainer-"+resolved.DevContainerID)
	if err != nil {
		return "", nil, fmt.Errorf("create Dev Container: %w", err)
	}
	if err := e.client.ContainerStart(ctx, response.ID, container.StartOptions{}); err != nil {
		_ = e.client.ContainerRemove(context.WithoutCancel(ctx), response.ID, container.RemoveOptions{Force: true})
		return "", nil, fmt.Errorf("start Dev Container: %w", err)
	}
	return response.ID, featureDigests, nil
}

func platformGPUArgs(arguments []string) ([]string, error) {
	result := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--gpus" {
			if index+1 >= len(arguments) || strings.TrimSpace(arguments[index+1]) != "all" {
				return nil, fmt.Errorf("runArgs --gpus must be all when the environment template allocates GPUs")
			}
			index++
			continue
		}
		if strings.HasPrefix(argument, "--gpus=") {
			if strings.TrimSpace(strings.TrimPrefix(argument, "--gpus=")) != "all" {
				return nil, fmt.Errorf("runArgs --gpus must be all when the environment template allocates GPUs")
			}
			continue
		}
		result = append(result, argument)
	}
	return result, nil
}

func (e *Engine) createContainerWithRunArgs(ctx context.Context, resolved *devcontainer.ResolvedConfiguration, config *container.Config, hostConfig *container.HostConfig, publishSpecs []string) (string, error) {
	arguments, err := dockerCreateArguments(resolved, config, hostConfig, publishSpecs)
	if err != nil {
		return "", err
	}

	var output bytes.Buffer
	cli, err := command.NewDockerCli(
		command.WithBaseContext(ctx),
		command.WithInputStream(io.NopCloser(strings.NewReader(""))),
		command.WithOutputStream(&output),
		command.WithErrorStream(e.stderr),
	)
	if err != nil {
		return "", fmt.Errorf("create Docker command client: %w", err)
	}
	if err := cli.Initialize(flags.NewClientOptions()); err != nil {
		return "", fmt.Errorf("initialize Docker command client: %w", err)
	}
	defer func() { _ = cli.Client().Close() }()
	//lint:ignore SA1019 The Docker CLI parser is required to preserve docker create semantics for devcontainer runArgs.
	createCommand := containercommand.NewCreateCommand(cli)
	createCommand.SetArgs(arguments)
	createCommand.SetContext(ctx)
	createCommand.SilenceUsage = true
	createCommand.SilenceErrors = true
	if err := createCommand.Execute(); err != nil {
		return "", fmt.Errorf("create Dev Container with runArgs: %w", err)
	}
	containerID := strings.TrimSpace(output.String())
	if containerID == "" {
		return "", errors.New("create Dev Container with runArgs returned no container ID")
	}
	if err := e.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = e.client.ContainerRemove(context.WithoutCancel(ctx), containerID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("start Dev Container: %w", err)
	}
	return containerID, nil
}

func dockerCreateArguments(resolved *devcontainer.ResolvedConfiguration, config *container.Config, hostConfig *container.HostConfig, publishSpecs []string) ([]string, error) {
	if slices.ContainsFunc(resolved.RunArgs, func(argument string) bool {
		return argument == "--entrypoint" || strings.HasPrefix(argument, "--entrypoint=")
	}) {
		return nil, fmt.Errorf("runArgs --entrypoint overrides Dev Container Feature startup entrypoints")
	}
	arguments := []string{"--tty", "--interactive"}
	if config.WorkingDir != "" {
		arguments = append(arguments, "--workdir", config.WorkingDir)
	}
	if config.User != "" {
		arguments = append(arguments, "--user", config.User)
	}
	for _, value := range config.Env {
		arguments = append(arguments, "--env", value)
	}
	for _, item := range hostConfig.Mounts {
		value := "type=" + string(item.Type) + ",target=" + item.Target
		if item.Source != "" {
			value += ",source=" + item.Source
		}
		if item.ReadOnly {
			value += ",readonly"
		}
		if item.Consistency != "" {
			value += ",consistency=" + string(item.Consistency)
		}
		if item.BindOptions != nil && item.BindOptions.Propagation != "" {
			value += ",bind-propagation=" + string(item.BindOptions.Propagation)
		}
		if item.VolumeOptions != nil && item.VolumeOptions.NoCopy {
			value += ",volume-nocopy"
		}
		arguments = append(arguments, "--mount", value)
	}
	if hostConfig.Privileged {
		arguments = append(arguments, "--privileged")
	}
	if hostConfig.Init != nil && *hostConfig.Init {
		arguments = append(arguments, "--init")
	}
	for _, value := range hostConfig.CapAdd {
		arguments = append(arguments, "--cap-add", value)
	}
	for _, value := range hostConfig.SecurityOpt {
		arguments = append(arguments, "--security-opt", value)
	}
	for _, mapping := range hostConfig.Devices {
		arguments = append(arguments, "--device", mapping.PathOnHost+":"+mapping.PathInContainer+":"+mapping.CgroupPermissions)
	}
	for _, request := range hostConfig.DeviceRequests {
		if request.Driver == "cdi" {
			for _, id := range request.DeviceIDs {
				arguments = append(arguments, "--device", id)
			}
		} else {
			arguments = append(arguments, "--gpus", "all")
		}
	}
	for _, spec := range publishSpecs {
		arguments = append(arguments, "--publish", spec)
	}
	arguments = append(arguments, resolved.RunArgs...)
	arguments = append(arguments, "--name", "devcontainer-"+resolved.DevContainerID)
	for _, name := range slices.Sorted(maps.Keys(config.Labels)) {
		arguments = append(arguments, "--label", name+"="+config.Labels[name])
	}
	if len(config.Entrypoint) > 0 {
		arguments = append(arguments, "--entrypoint", config.Entrypoint[0])
	}
	arguments = append(arguments, config.Image)
	arguments = append(arguments, config.Cmd...)
	return arguments, nil
}

func mergePlatformEnvironment(base, platform map[string]string) map[string]string {
	result := maps.Clone(base)
	if result == nil {
		result = map[string]string{}
	}
	maps.Copy(result, platform)
	var bypass []string
	seen := map[string]struct{}{}
	for _, value := range []string{base["NO_PROXY"], base["no_proxy"], platform["NO_PROXY"], platform["no_proxy"]} {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			key := strings.ToLower(item)
			if item == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			bypass = append(bypass, item)
		}
	}
	if len(bypass) > 0 {
		value := strings.Join(bypass, ",")
		result["NO_PROXY"], result["no_proxy"] = value, value
	}
	return result
}

func findRunArgsUser(arguments []string) string {
	for i := len(arguments) - 1; i >= 0; i-- {
		argument := arguments[i]
		if (argument == "-u" || argument == "--user") && i+1 < len(arguments) {
			return arguments[i+1]
		}
		if strings.HasPrefix(argument, "-u=") || strings.HasPrefix(argument, "--user=") {
			return argument[strings.IndexByte(argument, '=')+1:]
		}
	}
	return ""
}

func resolvePathInsideRoot(allowedRoot, value string) (string, error) {
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil {
		return "", err
	}
	if allowedRoot != "" {
		relative, err := filepath.Rel(allowedRoot, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path leaves the allowed path root")
		}
	}
	return resolved, nil
}

func (e *Engine) cleanupIncompleteCreate(ctx context.Context, ownerID, composeProject string) error {
	var result error
	if composeProject != "" {
		result = errors.Join(result, e.downComposeProject(ctx, composeProject))
	}
	query := filters.NewArgs(filters.Arg("label", labelOwnerID+"="+ownerID))
	containers, err := e.client.ContainerList(ctx, container.ListOptions{All: true, Filters: query})
	if err != nil {
		return errors.Join(result, err)
	}
	for _, item := range containers {
		if err := e.client.ContainerRemove(ctx, item.ID, container.RemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func appPortPublishSpecs(ports devcontainer.AppPortList) ([]string, error) {
	result := make([]string, 0, len(ports))
	for _, port := range ports {
		if port.Numeric || port.Number != 0 {
			if port.Number == 0 {
				continue
			}
			value := strconv.Itoa(int(port.Number))
			result = append(result, "127.0.0.1:"+value+":"+value)
			continue
		}
		value := strings.TrimSpace(port.Address)
		if value == "" {
			continue
		}
		result = append(result, value)
	}
	if _, _, err := nat.ParsePortSpecs(result); err != nil {
		return nil, fmt.Errorf("parse Docker port publish specs: %w", err)
	}
	return result, nil
}

func containerStartupScript(entrypoints []string) string {
	return "echo Container started\ntrap 'exit 0' TERM\n" + strings.Join(entrypoints, "\n") + "\nexec \"$@\"\nwhile sleep 1 & wait $!; do :; done"
}

func containerMounts(workspace, workspaceFolder, workspaceMount string, values []devcontainer.Mount) ([]mount.Mount, error) {
	configured, err := devContainerMounts(workspace, workspaceFolder, workspaceMount, values)
	if err != nil {
		return nil, err
	}
	result := make([]mount.Mount, 0, len(configured))
	for _, value := range configured {
		item := mount.Mount{Source: value.Source, Target: value.Target, Consistency: mount.Consistency(value.Consistency), ReadOnly: value.ReadOnly}
		switch value.Type {
		case "bind":
			item.Type = mount.TypeBind
			if value.BindPropagation != "" {
				item.BindOptions = &mount.BindOptions{Propagation: mount.Propagation(value.BindPropagation)}
			}
		case "volume":
			item.Type = mount.TypeVolume
			if value.VolumeNoCopy {
				item.VolumeOptions = &mount.VolumeOptions{NoCopy: true}
			}
		case "tmpfs":
			item.Type = mount.TypeTmpfs
		}
		result = append(result, item)
	}
	return result, nil
}

func devContainerMounts(workspace, workspaceFolder, workspaceMount string, values []devcontainer.Mount) ([]devcontainer.Mount, error) {
	workspaceConfig := devcontainer.Mount{Type: "bind", Source: workspace, Target: workspaceFolder}
	if strings.TrimSpace(workspaceMount) != "" {
		encoded, err := json.Marshal(workspaceMount)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &workspaceConfig); err != nil {
			return nil, fmt.Errorf("decode workspaceMount: %w", err)
		}
	}
	values = append([]devcontainer.Mount{workspaceConfig}, values...)
	resolved := make([]devcontainer.Mount, 0, len(values))
	for _, value := range values {
		if err := value.Validate(); err != nil {
			return nil, err
		}
		resolved = slices.DeleteFunc(resolved, func(existing devcontainer.Mount) bool { return existing.Target == value.Target })
		resolved = append(resolved, value)
	}
	return resolved, nil
}

type dockerProgressMessage struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Progress       string `json:"progress"`
	Stream         string `json:"stream"`
	Error          string `json:"error"`
	ProgressDetail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

func readDockerProgress(reader io.Reader, observe func(dockerProgressMessage)) error {
	decoder := json.NewDecoder(reader)
	for {
		var message dockerProgressMessage
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if message.ErrorDetail.Message != "" {
			return errors.New(message.ErrorDetail.Message)
		}
		if message.Error != "" {
			return errors.New(message.Error)
		}
		if observe != nil {
			observe(message)
		}
	}
}
