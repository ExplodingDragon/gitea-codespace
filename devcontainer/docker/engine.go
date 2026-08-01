// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	containercommand "github.com/docker/cli/cli/command/container"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	compose "github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/google/uuid"

	"gitea.dev/codespace/devcontainer"
)

const (
	labelOwnerID       = "devcontainer.owner_id"
	labelEnvironmentID = "devcontainer.environment_id"
	labelMetadata      = "devcontainer.metadata"
)

// Engine creates and controls one complete Dev Container environment.
type Engine struct {
	cli     *command.DockerCli
	client  client.APIClient
	compose api.Compose
	stdout  io.Writer
	stderr  io.Writer
}

// PrepareLifecycleFunc prepares caller-specific state after the remote user and
// environment are known and before the first in-container lifecycle command.
type PrepareLifecycleFunc func(context.Context, *Engine, *devcontainer.State) error

// ReadyFunc reports the first lifecycle stage selected by waitFor.
type ReadyFunc func(*devcontainer.State, devcontainer.LifecycleStage)

// CreateOptions contains caller identity, configuration input, and product integration hooks.
type CreateOptions struct {
	OwnerID          string
	Workspace        string
	Source           devcontainer.Source
	AllowedPathRoot  string
	HostUser         devcontainer.HostUser
	LocalEnvironment map[string]string
	Secrets          map[string]string
	InjectedFeatures []devcontainer.InjectedFeature
	AdditionalMounts []devcontainer.Mount
	Labels           map[string]string
	FrozenLockfile   bool
	Cache            devcontainer.CacheOptions
	PrepareLifecycle PrepareLifecycleFunc
	Ready            ReadyFunc
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
	return &Engine{
		cli:     cli,
		client:  cli.Client(),
		compose: compose.NewComposeService(cli, compose.WithPrompt(func(string, bool) (bool, error) { return true, nil })),
		stdout:  stdout,
		stderr:  stderr,
	}, nil
}

// Close releases the Docker client connection.
func (e *Engine) Close() error {
	return e.client.Close()
}

// Create resolves configuration, creates resources, and completes the first lifecycle.
func (e *Engine) Create(ctx context.Context, options CreateOptions) (*devcontainer.State, error) {
	if strings.TrimSpace(options.OwnerID) == "" {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("Dev Container owner ID is empty"))
	}
	environmentID := uuid.NewString()
	resolved, err := devcontainer.Load(devcontainer.LoadOptions{
		Workspace:       options.Workspace,
		Source:          options.Source,
		LocalEnv:        options.LocalEnvironment,
		ID:              environmentID,
		AllowedPathRoot: options.AllowedPathRoot,
	})
	if err != nil {
		return nil, devcontainer.InvalidConfiguration(err)
	}
	if err := validateCacheOptions(options.Cache); err != nil {
		return nil, devcontainer.InvalidConfiguration(err)
	}
	composeProject := ""
	if len(resolved.DockerComposeFile) > 0 {
		composeProject = composeProjectName(options.OwnerID)
	}
	if err := e.cleanupIncompleteCreate(ctx, options.OwnerID, composeProject); err != nil {
		return nil, fmt.Errorf("clean incomplete Dev Container environment: %w", err)
	}
	if err := mergeInjectedFeatures(resolved, options.InjectedFeatures); err != nil {
		return nil, devcontainer.InvalidConfiguration(err)
	}
	resolved.FrozenLockfile = options.FrozenLockfile
	resolved.Cache = options.Cache
	resolved.Configuration = devcontainer.Merge(resolved.Configuration, devcontainer.Configuration{Mounts: options.AdditionalMounts})
	if err := checkHostRequirements(resolved.HostRequirements, resolved.Workspace); err != nil {
		return nil, devcontainer.InvalidConfiguration(err)
	}
	if err := runInitializeCommand(ctx, resolved.InitializeCommand, options.HostUser, resolved.Workspace, options.LocalEnvironment, e.stdout, e.stderr); err != nil {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("run initializeCommand: %w", err))
	}

	workspaceFolder := strings.TrimSpace(resolved.WorkspaceFolder)
	if workspaceFolder == "" {
		workspaceFolder = filepath.Join(devcontainer.DefaultWorkspaceFolder, filepath.Base(resolved.Workspace))
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
	if len(resolved.DockerComposeFile) > 0 {
		primaryService = resolved.Service
		primaryID, related, featureDigests, err = e.createCompose(ctx, resolved, composeProject, workspaceFolder, labels, options.HostUser)
	} else {
		primaryID, featureDigests, err = e.createContainer(ctx, resolved, workspaceFolder, labels, options.HostUser)
	}
	if err != nil {
		return nil, err
	}
	environment := &devcontainer.State{
		Version:             devcontainer.StateFormatVersion,
		ID:                  environmentID,
		OwnerID:             options.OwnerID,
		ConfigurationPath:   resolved.ConfigurationPath,
		ConfigurationSHA256: resolved.ContentSHA256,
		Configuration:       resolved.Configuration,
		Workspace:           resolved.Workspace,
		WorkspaceFolder:     workspaceFolder,
		ComposeProject:      composeProject,
		PrimaryService:      primaryService,
		PrimaryContainerID:  primaryID,
		RelatedContainerIDs: related,
		RemoteUser:          strings.TrimSpace(resolved.RemoteUser),
		RemoteWorkdir:       workspaceFolder,
		RemoteEnvironment:   map[string]string{},
		FeatureDigests:      featureDigests,
	}
	if err := e.initializeContainer(ctx, environment, options.Secrets, options.PrepareLifecycle, options.Ready); err != nil {
		_ = e.Delete(context.WithoutCancel(ctx), environment)
		return nil, err
	}
	if err := environment.Validate(); err != nil {
		_ = e.Delete(context.WithoutCancel(ctx), environment)
		return nil, err
	}
	return environment, nil
}

func mergeInjectedFeatures(resolved *devcontainer.ResolvedConfiguration, injected []devcontainer.InjectedFeature) error {
	resolved.InjectedFeatureReferences = map[string]struct{}{}
	resolved.InstallOnlyFeatures = map[string]struct{}{}
	featureOrigins := make(map[string]string, len(resolved.Features)+len(injected))
	featureIDs := make(map[string]string, len(resolved.Features)+len(injected))
	for reference := range resolved.Features {
		featureOrigins[reference] = "repository"
		featureID, err := featureReferenceID(reference)
		if err != nil {
			return err
		}
		if existing, ok := featureIDs[featureID]; ok {
			return fmt.Errorf("Dev Container Feature %s conflicts with %s in repository configuration", reference, existing)
		}
		featureIDs[featureID] = reference
	}
	for _, feature := range injected {
		reference := strings.TrimSpace(feature.Reference)
		origin := strings.TrimSpace(feature.Origin)
		if reference == "" || origin == "" {
			return fmt.Errorf("injected Dev Container Feature identity is incomplete")
		}
		featureOptions, err := json.Marshal(feature.Options)
		if err != nil {
			return fmt.Errorf("encode %s Feature %s options: %w", origin, reference, err)
		}
		if resolved.Features == nil {
			resolved.Features = map[string]json.RawMessage{}
		}
		if existing, ok := resolved.Features[reference]; ok {
			if !featureOptionsEqual(existing, featureOptions) {
				return fmt.Errorf("Dev Container Feature %s conflicts between %s and %s", reference, featureOrigins[reference], origin)
			}
			if feature.InstallOnly {
				resolved.InstallOnlyFeatures[reference] = struct{}{}
			}
			continue
		}
		featureID, err := featureReferenceID(reference)
		if err != nil {
			return err
		}
		if existing, ok := featureIDs[featureID]; ok {
			return fmt.Errorf("Dev Container Feature %s from %s conflicts with %s from %s", reference, origin, existing, featureOrigins[existing])
		}
		resolved.Features[reference] = featureOptions
		resolved.InjectedFeatureReferences[reference] = struct{}{}
		if feature.InstallOnly {
			resolved.InstallOnlyFeatures[reference] = struct{}{}
		}
		featureOrigins[reference] = origin
		featureIDs[featureID] = reference
	}
	return nil
}

func featureOptionsEqual(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	normalizeEmpty := func(value any) any {
		if value == nil || value == true {
			return map[string]any{}
		}
		return value
	}
	return reflect.DeepEqual(normalizeEmpty(leftValue), normalizeEmpty(rightValue))
}

func (e *Engine) createContainer(ctx context.Context, resolved *devcontainer.ResolvedConfiguration, workspaceFolder string, labels map[string]string, hostUser devcontainer.HostUser) (string, map[string]string, error) {
	imageName, featureDigests, err := e.resolveImage(ctx, resolved)
	if err != nil {
		return "", nil, err
	}
	imageName, err = e.prepareUserImage(ctx, imageName, resolved, hostUser)
	if err != nil {
		return "", nil, err
	}
	mounts, err := containerMounts(resolved.Workspace, workspaceFolder, resolved.WorkspaceMount, resolved.Mounts)
	if err != nil {
		return "", nil, err
	}
	useGPU, err := e.resolveGPURequest(ctx, resolved.HostRequirements.GPU)
	if err != nil {
		return "", nil, devcontainer.InvalidConfiguration(err)
	}
	override := resolved.OverrideCommand == nil || *resolved.OverrideCommand
	config := &container.Config{
		Image:      imageName,
		Env:        environmentList(resolved.ContainerEnv),
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
			inspect, err := e.client.ImageInspect(ctx, imageName)
			if err != nil {
				return "", nil, fmt.Errorf("inspect Dev Container command: %w", err)
			}
			if inspect.Config != nil {
				arguments = append(arguments, inspect.Config.Entrypoint...)
				arguments = append(arguments, inspect.Config.Cmd...)
			}
		}
		config.Entrypoint = []string{"/bin/sh", "-c"}
		config.Cmd = append([]string{containerStartupScript(resolved.FeatureEntrypoints), "-"}, arguments...)
	}
	hostConfig := &container.HostConfig{
		Mounts:      mounts,
		Privileged:  resolved.Privileged,
		CapAdd:      slices.Clone(resolved.CapAdd),
		SecurityOpt: slices.Clone(resolved.SecurityOpt),
		Init:        &resolved.Init,
	}
	if useGPU {
		hostConfig.Resources.DeviceRequests = []container.DeviceRequest{{Count: -1, Capabilities: [][]string{{"gpu"}}}}
	}
	if len(resolved.RunArgs) > 0 {
		containerID, err := e.createContainerWithRunArgs(ctx, resolved, config, hostConfig)
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

func (e *Engine) createContainerWithRunArgs(ctx context.Context, resolved *devcontainer.ResolvedConfiguration, config *container.Config, hostConfig *container.HostConfig) (string, error) {
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
	if len(hostConfig.Resources.DeviceRequests) > 0 {
		arguments = append(arguments, "--gpus", "all")
	}
	arguments = append(arguments, resolved.RunArgs...)
	arguments = append(arguments, "--name", "devcontainer-"+resolved.DevContainerID)
	labelNames := make([]string, 0, len(config.Labels))
	for name := range config.Labels {
		labelNames = append(labelNames, name)
	}
	sort.Strings(labelNames)
	for _, name := range labelNames {
		arguments = append(arguments, "--label", name+"="+config.Labels[name])
	}
	if len(config.Entrypoint) > 0 {
		arguments = append(arguments, "--entrypoint", config.Entrypoint[0])
	}
	arguments = append(arguments, config.Image)
	arguments = append(arguments, config.Cmd...)

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
	defer cli.Client().Close()
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

func (e *Engine) createCompose(ctx context.Context, resolved *devcontainer.ResolvedConfiguration, projectName, workspaceFolder string, labels map[string]string, hostUser devcontainer.HostUser) (string, []string, map[string]string, error) {
	files := make([]string, 0, len(resolved.DockerComposeFile))
	for _, value := range resolved.DockerComposeFile {
		var file string
		if filepath.IsAbs(value) {
			file = value
		} else {
			file = filepath.Join(resolved.ConfigurationDir, value)
		}
		file, err := resolvePathInsideRoot(resolved.AllowedPathRoot, file)
		if err != nil {
			return "", nil, nil, fmt.Errorf("resolve Docker Compose file %s: %w", value, err)
		}
		files = append(files, file)
	}
	options, err := composecli.NewProjectOptions(files,
		composecli.WithName(projectName),
		composecli.WithWorkingDirectory(resolved.ConfigurationDir),
		composecli.WithResolvedPaths(true),
	)
	if err != nil {
		return "", nil, nil, fmt.Errorf("prepare Docker Compose project: %w", err)
	}
	composeEnvironment := map[string]string{}
	for _, item := range os.Environ() {
		if name, value, ok := strings.Cut(item, "="); ok {
			composeEnvironment[name] = value
		}
	}
	for name, value := range resolved.LocalEnvironment {
		composeEnvironment[name] = value
	}
	options.Environment = composetypes.Mapping(composeEnvironment)
	project, err := options.LoadProject(ctx)
	if err != nil {
		return "", nil, nil, fmt.Errorf("load Docker Compose project: %w", err)
	}
	for name, candidate := range project.Services {
		if candidate.Build == nil {
			continue
		}
		if _, err := resolvePathInsideRoot(resolved.AllowedPathRoot, candidate.Build.Context); err != nil {
			return "", nil, nil, fmt.Errorf("Docker Compose service %s build context: %w", name, err)
		}
	}
	service, ok := project.Services[resolved.Service]
	if !ok {
		return "", nil, nil, fmt.Errorf("Docker Compose service %q does not exist", resolved.Service)
	}
	if service.Build != nil {
		stage := "compose-" + resolved.Service
		if err := e.buildService(ctx, project, resolved.Service, buildCacheReference(resolved.Cache, stage), stage); err != nil {
			return "", nil, nil, fmt.Errorf("build Docker Compose Dev Container service: %w", err)
		}
	}
	baseImage := api.GetImageNameOrDefault(service, project.Name)
	if strings.TrimSpace(baseImage) == "" {
		return "", nil, nil, fmt.Errorf("Docker Compose service %q has no image or build", resolved.Service)
	}
	if service.Build == nil {
		baseImage, err = e.resolveAndPullImage(ctx, baseImage, resolved.Cache)
		if err != nil {
			return "", nil, nil, err
		}
	}
	repositoryConfiguration := resolved.Configuration
	metadata, err := e.readImageMetadata(ctx, baseImage)
	if err != nil {
		return "", nil, nil, err
	}
	imageConfiguration := metadata.Configuration
	resolved.FeatureEntrypoints = append(resolved.FeatureEntrypoints, metadata.Entrypoints...)
	if strings.TrimSpace(repositoryConfiguration.ContainerUser) == "" && strings.TrimSpace(service.User) != "" {
		repositoryConfiguration.ContainerUser = service.User
	}
	resolved.Configuration = devcontainer.Merge(imageConfiguration, repositoryConfiguration)
	featureImage, featureDigests, err := e.applyFeatures(ctx, baseImage, resolved, imageConfiguration, repositoryConfiguration)
	if err != nil {
		return "", nil, nil, err
	}
	featureImage, err = e.prepareUserImage(ctx, featureImage, resolved, hostUser)
	if err != nil {
		return "", nil, nil, err
	}
	useGPU, err := e.resolveGPURequest(ctx, resolved.HostRequirements.GPU)
	if err != nil {
		return "", nil, nil, devcontainer.InvalidConfiguration(err)
	}
	service.Image = featureImage
	service.Build = nil
	configuredMounts := slices.Clone(resolved.Mounts)
	overriddenVolumeTargets := map[string]struct{}{}
	for _, item := range configuredMounts {
		if err := item.Validate(); err != nil {
			return "", nil, nil, err
		}
		overriddenVolumeTargets[item.Target] = struct{}{}
	}
	existingVolumes := slices.Clone(service.Volumes)
	volumes := service.Volumes[:0]
	for _, volume := range existingVolumes {
		if _, overridden := overriddenVolumeTargets[volume.Target]; !overridden {
			volumes = append(volumes, volume)
		}
	}
	service.Volumes = volumes
	for _, item := range configuredMounts {
		volume := composetypes.ServiceVolumeConfig{Type: item.Type, Source: item.Source, Target: item.Target, Consistency: item.Consistency, ReadOnly: item.ReadOnly}
		if item.BindPropagation != "" {
			volume.Bind = &composetypes.ServiceVolumeBind{Propagation: item.BindPropagation}
		}
		if item.VolumeNoCopy {
			volume.Volume = &composetypes.ServiceVolumeVolume{NoCopy: true}
		}
		service.Volumes = append(service.Volumes, volume)
	}
	if service.Environment == nil {
		service.Environment = composetypes.MappingWithEquals{}
	}
	for name, value := range resolved.ContainerEnv {
		service.Environment[name] = &value
	}
	if service.Labels == nil {
		service.Labels = composetypes.Labels{}
	}
	for name, value := range labels {
		service.Labels[name] = value
	}
	if resolved.Privileged {
		service.Privileged = true
	}
	if useGPU {
		service.Gpus = []composetypes.DeviceRequest{{Count: -1, Capabilities: []string{"gpu"}}}
	}
	service.User = resolved.ContainerUser
	if resolved.Init {
		init := true
		service.Init = &init
	}
	service.CapAdd = append(service.CapAdd, resolved.CapAdd...)
	service.SecurityOpt = append(service.SecurityOpt, resolved.SecurityOpt...)
	overrideCommand := resolved.OverrideCommand != nil && *resolved.OverrideCommand
	if overrideCommand || len(resolved.FeatureEntrypoints) > 0 {
		var entrypoint, command composetypes.ShellCommand
		if !overrideCommand {
			entrypoint = slices.Clone(service.Entrypoint)
			command = slices.Clone(service.Command)
		}
		service.Entrypoint = append(composetypes.ShellCommand{"/bin/sh", "-c", containerStartupScript(resolved.FeatureEntrypoints), "-"}, entrypoint...)
		service.Command = command
	}
	project.Services[resolved.Service] = service
	for name, relatedService := range project.Services {
		relatedService.CustomLabels = composetypes.Labels{
			api.ProjectLabel:     project.Name,
			api.ServiceLabel:     name,
			api.VersionLabel:     api.ComposeVersion,
			api.WorkingDirLabel:  project.WorkingDir,
			api.ConfigFilesLabel: strings.Join(project.ComposeFiles, ","),
			api.OneoffLabel:      "False",
		}
		if relatedService.Labels == nil {
			relatedService.Labels = composetypes.Labels{}
		}
		for label, value := range labels {
			relatedService.Labels[label] = value
		}
		project.Services[name] = relatedService
	}
	services := slices.Clone(resolved.RunServices)
	if len(services) > 0 && !slices.Contains(services, resolved.Service) {
		services = append(services, resolved.Service)
	}
	if len(services) == 0 {
		services = project.ServiceNames()
	}
	for _, name := range services {
		if name == resolved.Service {
			continue
		}
		relatedService, ok := project.Services[name]
		if !ok {
			return "", nil, nil, devcontainer.InvalidConfiguration(fmt.Errorf("Docker Compose runServices service %q does not exist", name))
		}
		if relatedService.Build != nil {
			stage := "compose-" + name
			if err := e.buildService(ctx, project, name, buildCacheReference(resolved.Cache, stage), stage); err != nil {
				return "", nil, nil, fmt.Errorf("build Docker Compose service %s: %w", name, err)
			}
			continue
		}
		if strings.TrimSpace(relatedService.Image) == "" {
			continue
		}
		localImage, err := e.resolveAndPullImage(ctx, relatedService.Image, resolved.Cache)
		if err != nil {
			return "", nil, nil, fmt.Errorf("pull Docker Compose service %s image: %w", name, err)
		}
		relatedService.Image = localImage
		project.Services[name] = relatedService
	}
	if err := e.compose.Up(ctx, project, api.UpOptions{
		Create: api.CreateOptions{Services: services, RemoveOrphans: true, AssumeYes: true},
		Start:  api.StartOptions{Project: project, Services: services, Wait: true, WaitTimeout: 2 * time.Minute},
	}); err != nil {
		_ = e.downComposeProject(context.WithoutCancel(ctx), projectName)
		return "", nil, nil, fmt.Errorf("start Docker Compose Dev Container: %w", err)
	}
	containers, err := e.compose.Ps(ctx, projectName, api.PsOptions{All: true})
	if err != nil {
		_ = e.downComposeProject(context.WithoutCancel(ctx), projectName)
		return "", nil, nil, fmt.Errorf("inspect Docker Compose Dev Container: %w", err)
	}
	var primary string
	related := make([]string, 0, len(containers)-1)
	for _, item := range containers {
		if item.Service == resolved.Service {
			primary = item.ID
		} else {
			related = append(related, item.ID)
		}
	}
	if primary == "" {
		_ = e.downComposeProject(context.WithoutCancel(ctx), projectName)
		return "", nil, nil, fmt.Errorf("Docker Compose service %q did not create a container", resolved.Service)
	}
	sort.Strings(related)
	return primary, related, featureDigests, nil
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

func (e *Engine) initializeContainer(ctx context.Context, environment *devcontainer.State, secrets map[string]string, prepareLifecycle PrepareLifecycleFunc, ready ReadyFunc) error {
	inspect, err := e.client.ContainerInspect(ctx, environment.PrimaryContainerID)
	if err != nil {
		return fmt.Errorf("inspect Dev Container: %w", err)
	}
	if environment.RemoteUser == "" {
		environment.RemoteUser = strings.TrimSpace(inspect.Config.User)
		if environment.RemoteUser == "" {
			environment.RemoteUser = "root"
		}
	}
	remoteEnv, err := e.probeRemoteEnvironment(ctx, environment)
	if err != nil {
		return err
	}
	for name, value := range environment.Configuration.RemoteEnv {
		if value == nil {
			delete(remoteEnv, name)
		} else {
			remoteEnv[name] = *value
		}
	}
	environment.RemoteEnvironment = remoteEnv
	if prepareLifecycle != nil {
		if err := prepareLifecycle(ctx, e, environment); err != nil {
			return fmt.Errorf("prepare Dev Container lifecycle: %w", err)
		}
	}
	readySent := false
	emitReady := func(stage devcontainer.LifecycleStage) {
		if !readySent && environment.Configuration.WaitFor == stage {
			readySent = true
			if ready != nil {
				ready(environment, stage)
			}
		}
	}
	emitReady(devcontainer.LifecycleStageInitialize)
	commands := []struct {
		name    devcontainer.LifecycleStage
		command devcontainer.Command
		mark    *bool
	}{
		{name: devcontainer.LifecycleStageOnCreate, command: environment.Configuration.OnCreateCommand, mark: &environment.Lifecycle.OnCreateComplete},
		{name: devcontainer.LifecycleStageUpdateContent, command: environment.Configuration.UpdateContentCommand, mark: &environment.Lifecycle.UpdateContentComplete},
		{name: devcontainer.LifecycleStagePostCreate, command: environment.Configuration.PostCreateCommand, mark: &environment.Lifecycle.PostCreateComplete},
		{name: devcontainer.LifecycleStagePostStart, command: environment.Configuration.PostStartCommand},
	}
	for _, item := range commands {
		if err := e.runLifecycleCommand(ctx, environment, string(item.name), item.command, secrets); err != nil {
			return err
		}
		if item.mark != nil {
			*item.mark = true
		}
		emitReady(item.name)
	}
	return nil
}

// Start resumes an existing environment and runs postStartCommand.
func (e *Engine) Start(ctx context.Context, environment *devcontainer.State, secrets map[string]string) (*devcontainer.State, error) {
	if err := environment.Validate(); err != nil {
		return nil, err
	}
	if environment.ComposeProject != "" {
		if err := e.compose.Start(ctx, environment.ComposeProject, api.StartOptions{Wait: true, WaitTimeout: 2 * time.Minute}); err != nil {
			return nil, fmt.Errorf("resume Docker Compose Dev Container: %w", err)
		}
	} else {
		ids := append(slices.Clone(environment.RelatedContainerIDs), environment.PrimaryContainerID)
		for _, id := range ids {
			inspect, err := e.client.ContainerInspect(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("inspect Dev Container %s: %w", id, err)
			}
			if !inspect.State.Running {
				if err := e.client.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
					return nil, fmt.Errorf("resume Dev Container %s: %w", id, err)
				}
			}
		}
	}
	if err := e.runLifecycleCommand(ctx, environment, string(devcontainer.LifecycleStagePostStart), environment.Configuration.PostStartCommand, secrets); err != nil {
		return nil, err
	}
	return e.Inspect(ctx, environment)
}

// Stop stops all resources belonging to an environment without deleting them.
func (e *Engine) Stop(ctx context.Context, environment *devcontainer.State) (*devcontainer.State, error) {
	if err := environment.Validate(); err != nil {
		return nil, err
	}
	timeout := 20 * time.Second
	if environment.ComposeProject != "" {
		if err := e.compose.Stop(ctx, environment.ComposeProject, api.StopOptions{Timeout: &timeout}); err != nil {
			return nil, fmt.Errorf("stop Docker Compose Dev Container: %w", err)
		}
		return environment, nil
	}
	timeoutSeconds := int(timeout.Seconds())
	for _, id := range environmentContainerIDs(environment) {
		if err := e.client.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeoutSeconds}); err != nil && !client.IsErrNotFound(err) {
			return nil, fmt.Errorf("stop Dev Container %s: %w", id, err)
		}
	}
	return environment, nil
}

// Inspect verifies resource identity and requires the primary container to be running.
func (e *Engine) Inspect(ctx context.Context, environment *devcontainer.State) (*devcontainer.State, error) {
	for _, id := range environmentContainerIDs(environment) {
		inspect, err := e.client.ContainerInspect(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("inspect Dev Container %s: %w", id, err)
		}
		if id == environment.PrimaryContainerID && !inspect.State.Running {
			return nil, fmt.Errorf("Dev Container %s is not running", id)
		}
		if inspect.Config.Labels[labelOwnerID] != environment.OwnerID || inspect.Config.Labels[labelEnvironmentID] != environment.ID {
			return nil, fmt.Errorf("Dev Container %s identity does not match runtime state", id)
		}
	}
	return environment, nil
}

// Delete removes all resources belonging to an environment.
func (e *Engine) Delete(ctx context.Context, environment *devcontainer.State) error {
	var result error
	if environment.ComposeProject != "" {
		result = errors.Join(result, e.downComposeProject(ctx, environment.ComposeProject))
	}
	for _, id := range environmentContainerIDs(environment) {
		if id == "" {
			continue
		}
		if err := e.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil && !client.IsErrNotFound(err) {
			result = errors.Join(result, err)
		}
	}
	return result
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
		if err := e.client.ContainerRemove(ctx, item.ID, container.RemoveOptions{Force: true}); err != nil && !client.IsErrNotFound(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (e *Engine) downComposeProject(ctx context.Context, projectName string) error {
	query := filters.NewArgs(filters.Arg("label", api.ProjectLabel+"="+projectName))
	containers, err := e.client.ContainerList(ctx, container.ListOptions{All: true, Filters: query})
	if err != nil {
		return err
	}
	hasResources := len(containers) > 0
	if !hasResources {
		networks, err := e.client.NetworkList(ctx, network.ListOptions{Filters: query})
		if err != nil {
			return err
		}
		hasResources = len(networks) > 0
	}
	if !hasResources {
		volumes, err := e.client.VolumeList(ctx, volume.ListOptions{Filters: query})
		if err != nil {
			return err
		}
		hasResources = len(volumes.Volumes) > 0
	}
	if !hasResources {
		return nil
	}
	return e.compose.Down(ctx, projectName, api.DownOptions{RemoveOrphans: true})
}

func composeProjectName(ownerID string) string {
	digest := sha256.Sum256([]byte(ownerID))
	return fmt.Sprintf("devcontainer-%x", digest[:12])
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

func environmentList(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}

func environmentContainerIDs(environment *devcontainer.State) []string {
	return append([]string{environment.PrimaryContainerID}, environment.RelatedContainerIDs...)
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

func (m dockerProgressMessage) err() error {
	if m.ErrorDetail.Message != "" {
		return errors.New(m.ErrorDetail.Message)
	}
	if m.Error != "" {
		return errors.New(m.Error)
	}
	return nil
}
