// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"gitea.dev/codespace/devcontainer"
)

func composeProjectName(ownerID string) string {
	digest := sha256.Sum256([]byte(ownerID))
	return fmt.Sprintf("devcontainer-%x", digest[:12])
}

func (e *Engine) createCompose(ctx context.Context, resolved *devcontainer.ResolvedConfiguration, projectName string, labels map[string]string, hostUser devcontainer.HostUser, platformEnvironment map[string]string, platformDevices PlatformDevices, customizeImage CustomizeImageFunc) (string, []string, map[string]string, map[string]string, error) {
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
			return "", nil, nil, nil, fmt.Errorf("resolve Docker Compose file %s: %w", value, err)
		}
		files = append(files, file)
	}
	options, err := composecli.NewProjectOptions(files,
		composecli.WithName(projectName),
		composecli.WithWorkingDirectory(resolved.ConfigurationDir),
		composecli.WithResolvedPaths(true),
	)
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("prepare Docker Compose project: %w", err)
	}
	composeEnvironment := map[string]string{}
	for _, item := range os.Environ() {
		if name, value, ok := strings.Cut(item, "="); ok {
			composeEnvironment[name] = value
		}
	}
	maps.Copy(composeEnvironment, resolved.LocalEnvironment)
	options.Environment = composetypes.Mapping(composeEnvironment)
	project, err := options.LoadProject(ctx)
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("load Docker Compose project: %w", err)
	}
	platformEnvironment = composeProxyEnvironment(project.Services, platformEnvironment)
	for name, candidate := range project.Services {
		if candidate.Build == nil {
			continue
		}
		if _, err := resolvePathInsideRoot(resolved.AllowedPathRoot, candidate.Build.Context); err != nil {
			return "", nil, nil, nil, fmt.Errorf("docker compose service %s build context: %w", name, err)
		}
	}
	service, ok := project.Services[resolved.Service]
	if !ok {
		return "", nil, nil, nil, fmt.Errorf("docker compose service %q does not exist", resolved.Service)
	}
	if service.Build != nil {
		stage := "compose-" + resolved.Service
		if err := e.buildService(ctx, project, resolved.Service, resolved.Cache, stage, platformEnvironment); err != nil {
			return "", nil, nil, nil, fmt.Errorf("build Docker Compose Dev Container service: %w", err)
		}
	}
	baseImage := api.GetImageNameOrDefault(service, project.Name)
	if strings.TrimSpace(baseImage) == "" {
		return "", nil, nil, nil, fmt.Errorf("docker compose service %q has no image or build", resolved.Service)
	}
	if service.Build == nil {
		baseImage, err = e.resolveAndPullImage(ctx, baseImage, resolved.Cache)
		if err != nil {
			return "", nil, nil, nil, err
		}
	}
	repositoryConfiguration := resolved.Configuration
	metadata, err := e.readImageMetadata(ctx, baseImage)
	if err != nil {
		return "", nil, nil, nil, err
	}
	imageConfiguration := metadata.Configuration
	resolved.FeatureEntrypoints = append(resolved.FeatureEntrypoints, metadata.Entrypoints...)
	if strings.TrimSpace(repositoryConfiguration.ContainerUser) == "" && strings.TrimSpace(service.User) != "" {
		repositoryConfiguration.ContainerUser = service.User
	}
	resolved.Configuration = devcontainer.Merge(imageConfiguration, repositoryConfiguration)
	featureImage, featureDigests, err := e.applyFeatures(ctx, baseImage, resolved, imageConfiguration, metadata.Sources, repositoryConfiguration, platformEnvironment)
	if err != nil {
		return "", nil, nil, nil, err
	}
	featureImage, err = e.prepareUserImage(ctx, featureImage, resolved, hostUser)
	if err != nil {
		return "", nil, nil, nil, err
	}
	featureImage, err = e.customizeImage(ctx, featureImage, resolved, customizeImage, platformEnvironment)
	if err != nil {
		return "", nil, nil, nil, err
	}
	useGPU, err := e.resolveGPURequest(ctx, resolved.HostRequirements.GPU, platformDevices.ProvidesGPU)
	if err != nil {
		return "", nil, nil, nil, err
	}
	service.Image = featureImage
	service.Build = nil
	configuredMounts := slices.Clone(resolved.Mounts)
	overriddenVolumeTargets := map[string]struct{}{}
	for _, item := range configuredMounts {
		if err := item.Validate(); err != nil {
			return "", nil, nil, nil, err
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
	if err := applyPlatformDevicesToComposeService(&service, platformDevices); err != nil {
		return "", nil, nil, nil, err
	}
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
		maps.Copy(relatedService.Labels, labels)
		if relatedService.Environment == nil {
			relatedService.Environment = composetypes.MappingWithEquals{}
		}
		relatedEnvironment := make(map[string]string, len(relatedService.Environment))
		for environmentName, value := range relatedService.Environment {
			if value != nil {
				relatedEnvironment[environmentName] = *value
			}
		}
		for environmentName, value := range mergePlatformEnvironment(relatedEnvironment, platformEnvironment) {
			value := value
			relatedService.Environment[environmentName] = &value
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
			return "", nil, nil, nil, fmt.Errorf("docker compose runServices service %q does not exist", name)
		}
		if relatedService.Build != nil {
			stage := "compose-" + name
			if err := e.buildService(ctx, project, name, resolved.Cache, stage, platformEnvironment); err != nil {
				return "", nil, nil, nil, fmt.Errorf("build Docker Compose service %s: %w", name, err)
			}
			continue
		}
		if strings.TrimSpace(relatedService.Image) == "" {
			continue
		}
		localImage, err := e.resolveAndPullImage(ctx, relatedService.Image, resolved.Cache)
		if err != nil {
			return "", nil, nil, nil, fmt.Errorf("pull Docker Compose service %s image: %w", name, err)
		}
		relatedService.Image = localImage
		project.Services[name] = relatedService
	}
	if err := e.compose.Up(ctx, project, api.UpOptions{
		Create: api.CreateOptions{Services: services, RemoveOrphans: true, AssumeYes: true},
		Start:  api.StartOptions{Project: project, Services: services, Wait: true, WaitTimeout: 2 * time.Minute},
	}); err != nil {
		_ = e.downComposeProject(context.WithoutCancel(ctx), projectName)
		return "", nil, nil, nil, fmt.Errorf("start Docker Compose Dev Container: %w", err)
	}
	containers, err := e.compose.Ps(ctx, projectName, api.PsOptions{All: true})
	if err != nil {
		_ = e.downComposeProject(context.WithoutCancel(ctx), projectName)
		return "", nil, nil, nil, fmt.Errorf("inspect Docker Compose Dev Container: %w", err)
	}
	var primary string
	related := make([]string, 0, len(containers)-1)
	composeContainers := make(map[string]string, len(containers))
	for _, item := range containers {
		composeContainers[item.Service] = item.ID
		if item.Service == resolved.Service {
			primary = item.ID
		} else {
			related = append(related, item.ID)
		}
	}
	if primary == "" {
		_ = e.downComposeProject(context.WithoutCancel(ctx), projectName)
		return "", nil, nil, nil, fmt.Errorf("docker compose service %q did not create a container", resolved.Service)
	}
	slices.Sort(related)
	return primary, related, composeContainers, featureDigests, nil
}

func applyPlatformDevicesToComposeService(service *composetypes.ServiceConfig, devices PlatformDevices) error {
	var err error
	if devices.ProvidesGPU {
		service.Gpus, err = platformComposeGPURequests(service.Gpus, true)
		if err != nil {
			return err
		}
		if service.Deploy != nil && service.Deploy.Resources.Reservations != nil {
			service.Deploy.Resources.Reservations.Devices, err = platformComposeGPURequests(service.Deploy.Resources.Reservations.Devices, false)
			if err != nil {
				return err
			}
		}
	}
	for _, id := range devices.CDI {
		service.Devices = append(service.Devices, composetypes.DeviceMapping{Source: id, Target: id})
	}
	for _, mapping := range devices.Mappings {
		service.Devices = append(service.Devices, composetypes.DeviceMapping{Source: mapping.Source, Target: mapping.Target, Permissions: mapping.Permissions})
	}
	service.CapAdd = append(service.CapAdd, devices.Capabilities...)
	return nil
}

func platformComposeGPURequests(requests []composetypes.DeviceRequest, allGPU bool) ([]composetypes.DeviceRequest, error) {
	kept := make([]composetypes.DeviceRequest, 0, len(requests))
	for _, request := range requests {
		gpu := allGPU || request.Driver == "nvidia" || slices.Contains(request.Capabilities, "gpu")
		if !gpu {
			kept = append(kept, request)
			continue
		}
		if request.Count != -1 || len(request.IDs) != 0 || len(request.Options) != 0 || request.Driver != "" && request.Driver != "nvidia" ||
			len(request.Capabilities) > 0 && (len(request.Capabilities) != 1 || request.Capabilities[0] != "gpu") {
			return nil, fmt.Errorf("compose GPU selection must be all when the environment template allocates GPUs")
		}
	}
	return kept, nil
}

func composeProxyEnvironment(services composetypes.Services, platform map[string]string) map[string]string {
	proxyEnabled := false
	for name, value := range platform {
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
			proxyEnabled = proxyEnabled || strings.TrimSpace(value) != ""
		}
	}
	if !proxyEnabled {
		return platform
	}

	var destinations []string
	for name, service := range services {
		destinations = append(destinations, name)
		for _, network := range service.Networks {
			if network != nil {
				destinations = append(destinations, network.Aliases...)
			}
		}
	}
	slices.Sort(destinations)
	return mergePlatformEnvironment(map[string]string{"NO_PROXY": strings.Join(destinations, ",")}, platform)
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
