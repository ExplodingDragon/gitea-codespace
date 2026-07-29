// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	compose "github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
	dockerunits "github.com/docker/go-units"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"gitea.dev/codespace/internal/devcontainer"
)

const (
	labelCodespaceUUID = "dev.gitea.codespace.uuid"
	labelEnvironmentID = "dev.gitea.codespace.environment"
	runtimeBinaryPath  = "/usr/local/libexec/gitea-codespace-runtime"
	webIDEPort         = 13337
)

type runtimeMountSpec struct {
	path     string
	readOnly bool
}

var runtimeMounts = [...]runtimeMountSpec{
	{path: "/var/lib/gitea-codespace/gitea-token", readOnly: true},
	{path: "/var/lib/gitea-codespace/git", readOnly: true},
	{path: "/var/lib/gitea-codespace/bin", readOnly: true},
	{path: "/var/lib/gitea-codespace/runtime"},
}

// Engine creates and controls one complete Dev Container environment.
type Engine struct {
	cli     *command.DockerCli
	client  client.APIClient
	compose api.Compose
	stdout  io.Writer
	stderr  io.Writer
}

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

func (e *Engine) Close() error {
	return e.client.Close()
}

func (e *Engine) Apply(ctx context.Context, request devcontainer.RuntimeRequest) (*devcontainer.Environment, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	switch request.Action {
	case "create":
		return e.create(ctx, request)
	case "resume":
		return e.resume(ctx, request)
	case "stop":
		return e.stop(ctx, request)
	case "inspect":
		return e.inspect(ctx, request.Environment)
	default:
		panic("validated runtime action")
	}
}

func (e *Engine) create(ctx context.Context, request devcontainer.RuntimeRequest) (*devcontainer.Environment, error) {
	if err := e.cleanupIncompleteCreate(ctx, request.CodespaceUUID); err != nil {
		return nil, fmt.Errorf("clean incomplete Dev Container environment: %w", err)
	}
	environmentID := uuid.NewString()
	resolved, err := devcontainer.Load(request.Workspace, request.Selection, request.LocalEnvironment, environmentID)
	if err != nil {
		return nil, devcontainer.InvalidConfiguration(err)
	}
	if err := checkHostRequirements(resolved.HostRequirements, resolved.Workspace); err != nil {
		return nil, devcontainer.InvalidConfiguration(err)
	}
	if err := runInitializeCommand(ctx, resolved.InitializeCommand, request.RuntimeUser, resolved.Workspace, request.LocalEnvironment, e.stdout, e.stderr); err != nil {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("run initializeCommand: %w", err))
	}

	workspaceFolder := strings.TrimSpace(resolved.WorkspaceFolder)
	if workspaceFolder == "" {
		workspaceFolder = filepath.Join(devcontainer.DefaultWorkspaceFolder, filepath.Base(resolved.Workspace))
	}
	labels := map[string]string{
		labelCodespaceUUID: request.CodespaceUUID,
		labelEnvironmentID: environmentID,
	}

	var primaryID string
	var related []string
	var featureDigests map[string]string
	var composeProject string
	var primaryService string
	if len(resolved.DockerComposeFile) > 0 {
		composeProject = "gitea-" + strings.ReplaceAll(request.CodespaceUUID, "-", "")
		primaryService = resolved.Service
		primaryID, related, featureDigests, err = e.createCompose(ctx, resolved, composeProject, workspaceFolder, labels)
	} else {
		primaryID, featureDigests, err = e.createContainer(ctx, resolved, workspaceFolder, labels)
	}
	if err != nil {
		return nil, err
	}
	environment := &devcontainer.Environment{
		Version:             devcontainer.RuntimeFormatVersion,
		ID:                  environmentID,
		CodespaceUUID:       request.CodespaceUUID,
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
		WebIDEPort:          webIDEPort,
	}
	if err := writeConfiguredEndpoints(environment.Configuration); err != nil {
		_ = e.removeEnvironment(context.WithoutCancel(ctx), environment)
		return nil, devcontainer.InvalidConfiguration(err)
	}
	if err := e.initializeContainer(ctx, environment, request); err != nil {
		_ = e.removeEnvironment(context.WithoutCancel(ctx), environment)
		return nil, err
	}
	if err := environment.Validate(); err != nil {
		_ = e.removeEnvironment(context.WithoutCancel(ctx), environment)
		return nil, err
	}
	return environment, nil
}

func (e *Engine) createContainer(ctx context.Context, resolved *devcontainer.ResolvedConfiguration, workspaceFolder string, labels map[string]string) (string, map[string]string, error) {
	imageName, featureDigests, err := e.resolveImage(ctx, resolved)
	if err != nil {
		return "", nil, err
	}
	mounts, err := containerMounts(resolved.Workspace, workspaceFolder, resolved.WorkspaceMount, resolved.Mounts)
	if err != nil {
		return "", nil, err
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
	if override {
		config.Entrypoint = []string{"/bin/sh", "-c"}
		config.Cmd = []string{"trap 'exit 0' TERM; while sleep 1; do :; done"}
	}
	hostConfig := &container.HostConfig{
		Mounts:      mounts,
		Privileged:  resolved.Privileged,
		CapAdd:      slices.Clone(resolved.CapAdd),
		SecurityOpt: slices.Clone(resolved.SecurityOpt),
		Init:        &resolved.Init,
	}
	response, err := e.client.ContainerCreate(ctx, config, hostConfig, &network.NetworkingConfig{}, nil, "gitea-"+resolved.DevContainerID)
	if err != nil {
		return "", nil, fmt.Errorf("create Dev Container: %w", err)
	}
	if err := e.client.ContainerStart(ctx, response.ID, container.StartOptions{}); err != nil {
		_ = e.client.ContainerRemove(context.WithoutCancel(ctx), response.ID, container.RemoveOptions{Force: true})
		return "", nil, fmt.Errorf("start Dev Container: %w", err)
	}
	return response.ID, featureDigests, nil
}

func (e *Engine) createCompose(ctx context.Context, resolved *devcontainer.ResolvedConfiguration, projectName, workspaceFolder string, labels map[string]string) (string, []string, map[string]string, error) {
	files := make([]string, 0, len(resolved.DockerComposeFile))
	for _, value := range resolved.DockerComposeFile {
		var file string
		if filepath.IsAbs(value) {
			file = value
		} else {
			file = filepath.Join(resolved.ConfigurationDir, value)
		}
		file, err := resolveWorkspacePath(resolved.Workspace, file)
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
	options.Environment = composetypes.Mapping(resolved.ContainerEnv)
	project, err := options.LoadProject(ctx)
	if err != nil {
		return "", nil, nil, fmt.Errorf("load Docker Compose project: %w", err)
	}
	for name, candidate := range project.Services {
		if candidate.Build == nil {
			continue
		}
		if _, err := resolveWorkspacePath(resolved.Workspace, candidate.Build.Context); err != nil {
			return "", nil, nil, fmt.Errorf("Docker Compose service %s build context: %w", name, err)
		}
	}
	service, ok := project.Services[resolved.Service]
	if !ok {
		return "", nil, nil, fmt.Errorf("Docker Compose service %q does not exist", resolved.Service)
	}
	if service.Build != nil {
		if err := e.compose.Build(ctx, project, api.BuildOptions{Services: []string{resolved.Service}}); err != nil {
			return "", nil, nil, fmt.Errorf("build Docker Compose Dev Container service: %w", err)
		}
	}
	baseImage := api.GetImageNameOrDefault(service, project.Name)
	if strings.TrimSpace(baseImage) == "" {
		return "", nil, nil, fmt.Errorf("Docker Compose service %q has no image or build", resolved.Service)
	}
	featureImage, featureDigests, err := e.applyFeatures(ctx, baseImage, resolved)
	if err != nil {
		return "", nil, nil, err
	}
	service.Image = featureImage
	service.Build = nil
	for name, relatedService := range project.Services {
		if relatedService.Labels == nil {
			relatedService.Labels = composetypes.Labels{}
		}
		for label, value := range labels {
			relatedService.Labels[label] = value
		}
		project.Services[name] = relatedService
	}
	overriddenVolumeTargets := map[string]struct{}{workspaceFolder: {}}
	runtimeVolumeTargets := map[string]struct{}{}
	for _, item := range resolved.Mounts {
		overriddenVolumeTargets[item.Target] = struct{}{}
	}
	for _, spec := range runtimeMounts {
		if _, statErr := os.Stat(spec.path); statErr == nil {
			overriddenVolumeTargets[spec.path] = struct{}{}
			runtimeVolumeTargets[spec.path] = struct{}{}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", nil, nil, fmt.Errorf("inspect runtime mount %s: %w", spec.path, statErr)
		}
	}
	existingVolumes := slices.Clone(service.Volumes)
	volumes := service.Volumes[:0]
	for _, volume := range existingVolumes {
		if _, overridden := overriddenVolumeTargets[volume.Target]; !overridden {
			volumes = append(volumes, volume)
		}
	}
	service.Volumes = append(volumes, composetypes.ServiceVolumeConfig{Type: composetypes.VolumeTypeBind, Source: resolved.Workspace, Target: workspaceFolder})
	for _, spec := range runtimeMounts {
		if _, ok := runtimeVolumeTargets[spec.path]; ok {
			service.Volumes = append(service.Volumes, composetypes.ServiceVolumeConfig{Type: composetypes.VolumeTypeBind, Source: spec.path, Target: spec.path, ReadOnly: spec.readOnly})
		}
	}
	for _, item := range resolved.Mounts {
		service.Volumes = append(service.Volumes, composetypes.ServiceVolumeConfig{Type: item.Type, Source: item.Source, Target: item.Target, Consistency: item.Consistency})
	}
	if service.Environment == nil {
		service.Environment = composetypes.MappingWithEquals{}
	}
	for name, value := range resolved.ContainerEnv {
		value := value
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
	service.User = resolved.ContainerUser
	if resolved.Init {
		init := true
		service.Init = &init
	}
	service.CapAdd = append(service.CapAdd, resolved.CapAdd...)
	service.SecurityOpt = append(service.SecurityOpt, resolved.SecurityOpt...)
	if resolved.OverrideCommand != nil && *resolved.OverrideCommand {
		service.Entrypoint = composetypes.ShellCommand{"/bin/sh", "-c"}
		service.Command = composetypes.ShellCommand{"trap 'exit 0' TERM; while sleep 1; do :; done"}
	}
	project.Services[resolved.Service] = service
	services := slices.Clone(resolved.RunServices)
	if len(services) > 0 && !slices.Contains(services, resolved.Service) {
		services = append(services, resolved.Service)
	}
	if err := e.compose.Up(ctx, project, api.UpOptions{
		Create: api.CreateOptions{Services: services, RemoveOrphans: true, AssumeYes: true},
		Start:  api.StartOptions{Project: project, Services: services, Wait: true, WaitTimeout: 2 * time.Minute},
	}); err != nil {
		_ = e.compose.Down(context.WithoutCancel(ctx), projectName, api.DownOptions{RemoveOrphans: true})
		return "", nil, nil, fmt.Errorf("start Docker Compose Dev Container: %w", err)
	}
	containers, err := e.compose.Ps(ctx, projectName, api.PsOptions{All: true})
	if err != nil {
		_ = e.compose.Down(context.WithoutCancel(ctx), projectName, api.DownOptions{RemoveOrphans: true})
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
		_ = e.compose.Down(context.WithoutCancel(ctx), projectName, api.DownOptions{RemoveOrphans: true})
		return "", nil, nil, fmt.Errorf("Docker Compose service %q did not create a container", resolved.Service)
	}
	sort.Strings(related)
	return primary, related, featureDigests, nil
}

func (e *Engine) resolveImage(ctx context.Context, resolved *devcontainer.ResolvedConfiguration) (string, map[string]string, error) {
	var baseImage string
	if resolved.Image != "" {
		reader, err := e.client.ImagePull(ctx, resolved.Image, image.PullOptions{})
		if err != nil {
			return "", nil, fmt.Errorf("pull Dev Container image %s: %w", resolved.Image, err)
		}
		defer reader.Close()
		if err := streamDockerProgress(reader, e.stderr); err != nil {
			return "", nil, fmt.Errorf("read image pull progress: %w", err)
		}
		baseImage = resolved.Image
	} else {
		buildConfig := resolved.Build
		if buildConfig == nil {
			buildConfig = &devcontainer.Build{Dockerfile: resolved.DockerFile, Context: resolved.Context}
		}
		contextPath := buildConfig.Context
		if contextPath == "" {
			contextPath = "."
		}
		if !filepath.IsAbs(contextPath) {
			contextPath = filepath.Join(resolved.ConfigurationDir, contextPath)
		}
		contextPath, err := resolveWorkspacePath(resolved.Workspace, contextPath)
		if err != nil {
			return "", nil, fmt.Errorf("resolve Dev Container build context: %w", err)
		}
		dockerfile := buildConfig.Dockerfile
		if filepath.IsAbs(dockerfile) {
			relative, err := filepath.Rel(contextPath, dockerfile)
			if err != nil || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", nil, fmt.Errorf("Dev Container Dockerfile must be inside its build context")
			}
			dockerfile = relative
		}
		reader, err := archive.TarWithOptions(contextPath, &archive.TarOptions{})
		if err != nil {
			return "", nil, fmt.Errorf("archive Dev Container build context: %w", err)
		}
		defer reader.Close()
		imageName := "gitea-devcontainer:" + resolved.DevContainerID
		args := make(map[string]*string, len(buildConfig.Args))
		for name, value := range buildConfig.Args {
			value := value
			args[name] = &value
		}
		response, err := e.client.ImageBuild(ctx, reader, build.ImageBuildOptions{
			Dockerfile: dockerfile,
			Tags:       []string{imageName},
			BuildArgs:  args,
			Target:     buildConfig.Target,
			CacheFrom:  slices.Clone(buildConfig.CacheFrom),
			Remove:     true,
		})
		if err != nil {
			return "", nil, fmt.Errorf("build Dev Container image: %w", err)
		}
		defer response.Body.Close()
		if err := streamDockerProgress(response.Body, e.stderr); err != nil {
			return "", nil, fmt.Errorf("read image build progress: %w", err)
		}
		baseImage = imageName
	}
	return e.applyFeatures(ctx, baseImage, resolved)
}

func resolveWorkspacePath(workspace, value string) (string, error) {
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(workspace, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path leaves workspace")
	}
	return resolved, nil
}

func (e *Engine) initializeContainer(ctx context.Context, environment *devcontainer.Environment, request devcontainer.RuntimeRequest) error {
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
	if environment.Configuration.UpdateRemoteUserUID == nil || *environment.Configuration.UpdateRemoteUserUID {
		if err := e.updateRemoteUserIdentity(ctx, environment, request.RuntimeUser); err != nil {
			return err
		}
	}
	if err := e.copyRuntimeBinary(ctx, environment.PrimaryContainerID); err != nil {
		return err
	}
	remoteEnv, err := e.probeRemoteEnvironment(ctx, environment)
	if err != nil {
		return err
	}
	for name, value := range environment.Configuration.RemoteEnv {
		remoteEnv[name] = resolveContainerVariable(value, environment.WorkspaceFolder, inspect.Config.Env)
	}
	environment.RemoteEnvironment = remoteEnv
	commands := []struct {
		name    string
		command devcontainer.Command
		mark    *bool
	}{
		{name: "onCreateCommand", command: environment.Configuration.OnCreateCommand, mark: &environment.Lifecycle.OnCreateComplete},
		{name: "updateContentCommand", command: environment.Configuration.UpdateContentCommand, mark: &environment.Lifecycle.UpdateContentComplete},
		{name: "postCreateCommand", command: environment.Configuration.PostCreateCommand, mark: &environment.Lifecycle.PostCreateComplete},
		{name: "postStartCommand", command: environment.Configuration.PostStartCommand},
	}
	for _, item := range commands {
		if err := e.runLifecycleCommand(ctx, environment, item.name, item.command, request.Secrets); err != nil {
			return err
		}
		if item.mark != nil {
			*item.mark = true
		}
	}
	if err := e.configureGit(ctx, environment, request); err != nil {
		return err
	}
	return e.startWebIDE(ctx, environment, request.Secrets)
}

func (e *Engine) resume(ctx context.Context, request devcontainer.RuntimeRequest) (*devcontainer.Environment, error) {
	environment := request.Environment
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
	if err := e.runLifecycleCommand(ctx, environment, "postStartCommand", environment.Configuration.PostStartCommand, request.Secrets); err != nil {
		return nil, err
	}
	if err := e.startWebIDE(ctx, environment, request.Secrets); err != nil {
		return nil, err
	}
	return e.inspect(ctx, environment)
}

func (e *Engine) stop(ctx context.Context, request devcontainer.RuntimeRequest) (*devcontainer.Environment, error) {
	environment := request.Environment
	timeout := 20
	for _, id := range environmentContainerIDs(environment) {
		if err := e.client.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout}); err != nil && !client.IsErrNotFound(err) {
			return nil, fmt.Errorf("stop Dev Container %s: %w", id, err)
		}
	}
	return environment, nil
}

func (e *Engine) inspect(ctx context.Context, environment *devcontainer.Environment) (*devcontainer.Environment, error) {
	for _, id := range environmentContainerIDs(environment) {
		inspect, err := e.client.ContainerInspect(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("inspect Dev Container %s: %w", id, err)
		}
		if !inspect.State.Running {
			return nil, fmt.Errorf("Dev Container %s is not running", id)
		}
		if inspect.Config.Labels[labelCodespaceUUID] != environment.CodespaceUUID || inspect.Config.Labels[labelEnvironmentID] != environment.ID {
			return nil, fmt.Errorf("Dev Container %s identity does not match runtime state", id)
		}
	}
	return environment, nil
}

func (e *Engine) removeEnvironment(ctx context.Context, environment *devcontainer.Environment) error {
	var result error
	if environment.ComposeProject != "" {
		result = errors.Join(result, e.compose.Down(ctx, environment.ComposeProject, api.DownOptions{RemoveOrphans: true}))
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

func (e *Engine) cleanupIncompleteCreate(ctx context.Context, codespaceUUID string) error {
	query := filters.NewArgs(filters.Arg("label", labelCodespaceUUID+"="+codespaceUUID))
	containers, err := e.client.ContainerList(ctx, container.ListOptions{All: true, Filters: query})
	if err != nil {
		return err
	}
	var result error
	for _, item := range containers {
		result = errors.Join(result, e.client.ContainerRemove(ctx, item.ID, container.RemoveOptions{Force: true}))
	}
	projectName := "gitea-" + strings.ReplaceAll(codespaceUUID, "-", "")
	result = errors.Join(result, e.compose.Down(ctx, projectName, api.DownOptions{RemoveOrphans: true}))
	return result
}

func containerMounts(workspace, workspaceFolder, workspaceMount string, values []devcontainer.Mount) ([]mount.Mount, error) {
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
	result := []mount.Mount{{Type: mount.Type(workspaceConfig.Type), Source: workspaceConfig.Source, Target: workspaceConfig.Target, Consistency: mount.Consistency(workspaceConfig.Consistency)}}
	for _, spec := range runtimeMounts {
		if _, err := os.Stat(spec.path); err == nil {
			result = append(result, mount.Mount{Type: mount.TypeBind, Source: spec.path, Target: spec.path, ReadOnly: spec.readOnly})
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect runtime mount %s: %w", spec.path, err)
		}
	}
	for _, value := range values {
		item := mount.Mount{Source: value.Source, Target: value.Target, Consistency: mount.Consistency(value.Consistency)}
		switch value.Type {
		case "bind":
			item.Type = mount.TypeBind
		case "volume":
			item.Type = mount.TypeVolume
		case "tmpfs":
			item.Type = mount.TypeTmpfs
		default:
			return nil, fmt.Errorf("mount type %q is invalid", value.Type)
		}
		result = append(result, item)
	}
	return result, nil
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

func environmentContainerIDs(environment *devcontainer.Environment) []string {
	return append([]string{environment.PrimaryContainerID}, environment.RelatedContainerIDs...)
}

func streamDockerProgress(reader io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(reader)
	for {
		var message struct {
			Status      string `json:"status"`
			Progress    string `json:"progress"`
			Stream      string `json:"stream"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if message.Error != "" || message.ErrorDetail.Message != "" {
			if message.ErrorDetail.Message != "" {
				return errors.New(message.ErrorDetail.Message)
			}
			return errors.New(message.Error)
		}
		line := strings.TrimSpace(message.Stream)
		if line == "" {
			line = strings.TrimSpace(strings.TrimSpace(message.Status) + " " + strings.TrimSpace(message.Progress))
		}
		if line != "" {
			fmt.Fprintln(output, line)
		}
	}
}

func checkHostRequirements(requirements devcontainer.HostRequirements, workspace string) error {
	if requirements.CPUs < 0 {
		return fmt.Errorf("Dev Container hostRequirements.cpus must not be negative")
	}
	if requirements.CPUs > float64(runtime.NumCPU()) {
		return fmt.Errorf("Dev Container requires %.2f CPUs but the runtime provides %d", requirements.CPUs, runtime.NumCPU())
	}
	if strings.TrimSpace(requirements.Memory) != "" {
		required, err := dockerunits.RAMInBytes(requirements.Memory)
		if err != nil || required <= 0 {
			return fmt.Errorf("Dev Container hostRequirements.memory %q is invalid", requirements.Memory)
		}
		var available uint64
		if content, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil && strings.TrimSpace(string(content)) != "max" {
			available, _ = strconv.ParseUint(strings.TrimSpace(string(content)), 10, 64)
		}
		if available == 0 {
			var info unix.Sysinfo_t
			if err := unix.Sysinfo(&info); err != nil {
				return fmt.Errorf("inspect runtime memory: %w", err)
			}
			available = info.Totalram * uint64(info.Unit)
		}
		if uint64(required) > available {
			return fmt.Errorf("Dev Container requires %s memory but the runtime provides %s", requirements.Memory, dockerunits.BytesSize(float64(available)))
		}
	}
	if strings.TrimSpace(requirements.Storage) != "" {
		required, err := dockerunits.RAMInBytes(requirements.Storage)
		if err != nil || required <= 0 {
			return fmt.Errorf("Dev Container hostRequirements.storage %q is invalid", requirements.Storage)
		}
		var info unix.Statfs_t
		if err := unix.Statfs(workspace, &info); err != nil {
			return fmt.Errorf("inspect runtime storage: %w", err)
		}
		available := info.Blocks * uint64(info.Bsize)
		if uint64(required) > available {
			return fmt.Errorf("Dev Container requires %s storage but the runtime provides %s", requirements.Storage, dockerunits.BytesSize(float64(available)))
		}
	}
	gpu := strings.TrimSpace(string(requirements.GPU))
	if gpu != "" && gpu != "null" && gpu != "false" {
		return fmt.Errorf("Dev Container GPU host requirements are not supported by this runtime")
	}
	return nil
}

func resolveContainerVariable(value, workspaceFolder string, environment []string) string {
	values := map[string]string{}
	for _, item := range environment {
		name, value, ok := strings.Cut(item, "=")
		if ok {
			values[name] = value
		}
	}
	value = strings.ReplaceAll(value, "${containerWorkspaceFolder}", workspaceFolder)
	for name, setting := range values {
		value = strings.ReplaceAll(value, "${containerEnv:"+name+"}", setting)
	}
	return value
}
