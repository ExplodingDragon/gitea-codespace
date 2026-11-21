// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	dockerunits "github.com/docker/go-units"

	"gitea.dev/codespace/devcontainer"
)

//go:embed builtin/update-user.sh
var updateUserScript string

func (e *Engine) resolveBaseImage(ctx context.Context, resolved *devcontainer.ResolvedConfiguration, platformEnvironment map[string]string) (string, error) {
	if resolved.Image != "" {
		return e.resolveAndPullImage(ctx, resolved.Image, resolved.Cache)
	}
	buildConfig := resolved.Build
	contextPath := buildConfig.Context
	if contextPath == "" {
		contextPath = "."
	}
	if !filepath.IsAbs(contextPath) {
		contextPath = filepath.Join(resolved.ConfigurationDir, contextPath)
	}
	contextPath, err := resolvePathInsideRoot(resolved.AllowedPathRoot, contextPath)
	if err != nil {
		return "", fmt.Errorf("resolve Dev Container build context: %w", err)
	}
	dockerfile := buildConfig.Dockerfile
	if !filepath.IsAbs(dockerfile) {
		dockerfile = filepath.Join(resolved.ConfigurationDir, dockerfile)
	}
	dockerfile, err = resolvePathInsideRoot(resolved.AllowedPathRoot, dockerfile)
	if err != nil {
		return "", fmt.Errorf("resolve Dev Container Dockerfile: %w", err)
	}
	relativeDockerfile, err := filepath.Rel(contextPath, dockerfile)
	if err != nil || relativeDockerfile == ".." || strings.HasPrefix(relativeDockerfile, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("dev container Dockerfile must be inside its build context")
	}
	imageName := "devcontainer-build:" + resolved.DevContainerID
	args := make(map[string]*string, len(buildConfig.Args))
	for name, value := range buildConfig.Args {
		args[name] = &value
	}
	if err := e.buildImage(ctx, contextPath, relativeDockerfile, imageName, buildConfig.Target, args, buildConfig.CacheFrom, buildConfig.Options, resolved.Cache, "repository", platformEnvironment); err != nil {
		return "", fmt.Errorf("build Dev Container image: %w", err)
	}
	return imageName, nil
}

func (e *Engine) prepareImage(ctx context.Context, baseImage string, resolved *devcontainer.ResolvedConfiguration, hostUser devcontainer.HostUser, customize CustomizeImageFunc, platformEnvironment map[string]string) (string, map[string]string, error) {
	repositoryConfiguration := resolved.Configuration
	metadata, err := e.readImageMetadata(ctx, baseImage)
	if err != nil {
		return "", nil, err
	}
	imageConfiguration := metadata.Configuration
	resolved.FeatureEntrypoints = append(resolved.FeatureEntrypoints, metadata.Entrypoints...)
	resolved.Configuration = devcontainer.Merge(imageConfiguration, repositoryConfiguration)
	imageName, featureDigests, err := e.applyFeatures(ctx, baseImage, resolved, imageConfiguration, metadata.Sources, repositoryConfiguration, platformEnvironment)
	if err != nil {
		return "", nil, err
	}
	imageName, err = e.prepareUserImage(ctx, imageName, resolved, hostUser)
	if err != nil {
		return "", nil, err
	}
	imageName, err = e.customizeImage(ctx, imageName, resolved, customize, platformEnvironment)
	if err != nil {
		return "", nil, err
	}
	return imageName, featureDigests, nil
}

func (e *Engine) resolveAndPullImage(ctx context.Context, imageName string, cache devcontainer.CacheOptions) (string, error) {
	fetchReference, mirrored, err := mirroredReference(imageName, cache.Mirrors)
	if err != nil {
		return "", fmt.Errorf("resolve OCI mirror for %s: %w", imageName, err)
	}
	named, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		return "", err
	}
	if _, pinned := named.(reference.Digested); pinned {
		localReferences := []string{imageName}
		if fetchReference != imageName {
			localReferences = append(localReferences, fetchReference)
		}
		for _, localReference := range localReferences {
			available, err := e.localImageAvailable(ctx, localReference)
			if err != nil {
				return "", err
			}
			if available {
				_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Using local image %s\n", localReference)
				return localReference, nil
			}
		}
	}
	if mirrored {
		_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Pulling image %s through mirror %s\n", imageName, fetchReference)
		mirrorCtx, progress, finish := watchImageTransfer(ctx)
		pullErr := finish(e.pullImageReference(mirrorCtx, fetchReference, progress))
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if pullErr == nil {
			_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Pulled image %s through mirror %s\n", imageName, fetchReference)
			return fetchReference, nil
		}
		_, _ = fmt.Fprintf(e.stderr, "[WARN] [build] OCI mirror for %s is unavailable; falling back to the original registry: %v\n", imageName, pullErr)
	}
	_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Pulling image %s\n", imageName)
	if err := e.pullImageReference(ctx, imageName, nil); err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Pulled image %s\n", imageName)
	return imageName, nil
}

func (e *Engine) pullImageReference(ctx context.Context, imageName string, reportProgress func()) error {
	registryAuth, err := command.RetrieveAuthTokenFromImage(e.cli.ConfigFile(), imageName)
	if err != nil {
		return fmt.Errorf("resolve registry credentials for %s: %w", imageName, err)
	}
	reader, err := e.client.ImagePull(ctx, imageName, image.PullOptions{
		RegistryAuth: registryAuth,
		Platform:     e.imagePlatform(),
	})
	if err != nil {
		return fmt.Errorf("pull Dev Container image %s: %w", imageName, err)
	}
	defer func() { _ = reader.Close() }()
	observe := observeImageTransfer(reportProgress)
	if reportProgress != nil {
		reportProgress()
	}
	progress := dockerPullProgress{}
	if err := readDockerProgress(reader, func(message dockerProgressMessage) {
		if observe != nil {
			observe(message)
		}
		if summary := progress.observe(message, time.Now()); summary != "" {
			_, _ = fmt.Fprintln(e.stdout, "[INFO] [build]", summary)
		}
	}); err != nil {
		return fmt.Errorf("read image pull progress: %w", err)
	}
	return nil
}

func observeImageTransfer(progress func()) func(dockerProgressMessage) {
	if progress == nil {
		return nil
	}
	lastMessages := make(map[string]dockerProgressMessage)
	return func(message dockerProgressMessage) {
		if message.Status == "" {
			return
		}
		last, exists := lastMessages[message.ID]
		// Repeated progress messages do not mean the transfer is advancing.
		if !exists || message.Status != last.Status || message.ProgressDetail.Current > last.ProgressDetail.Current {
			progress()
		}
		lastMessages[message.ID] = message
	}
}

type dockerProgressMessage struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
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

const dockerPullProgressInterval = 10 * time.Second

type dockerPullLayer struct {
	current  int64
	total    int64
	complete bool
}

type dockerPullProgress struct {
	layers      map[string]dockerPullLayer
	lastReport  time.Time
	lastSummary string
}

func (p *dockerPullProgress) observe(message dockerProgressMessage, now time.Time) string {
	if message.ID == "" {
		return ""
	}
	if p.layers == nil {
		p.layers = make(map[string]dockerPullLayer)
	}
	layer := p.layers[message.ID]
	switch message.Status {
	case "Downloading":
		layer.current = message.ProgressDetail.Current
		layer.total = message.ProgressDetail.Total
	case "Download complete":
		if message.ProgressDetail.Total > 0 {
			layer.current = message.ProgressDetail.Total
			layer.total = message.ProgressDetail.Total
		} else if layer.total > 0 {
			layer.current = layer.total
		}
	case "Already exists", "Pull complete":
		layer.complete = true
	}
	p.layers[message.ID] = layer

	if p.lastReport.IsZero() {
		p.lastReport = now
		return ""
	}
	if now.Sub(p.lastReport) < dockerPullProgressInterval {
		return ""
	}
	p.lastReport = now

	var current, total int64
	complete := 0
	for _, layer := range p.layers {
		current += layer.current
		total += layer.total
		if layer.complete {
			complete++
		}
	}
	summary := fmt.Sprintf("Image pull progress: %d/%d layers complete", complete, len(p.layers))
	if total > 0 {
		summary += fmt.Sprintf(", %s / %s downloaded", dockerunits.HumanSize(float64(current)), dockerunits.HumanSize(float64(total)))
	}
	if summary == p.lastSummary {
		return ""
	}
	p.lastSummary = summary
	return summary
}

type imageMetadata struct {
	Configuration devcontainer.Configuration
	Sources       []devcontainer.Configuration
	Entrypoints   []string
}

type imageMetadataEntry struct {
	Entrypoint           string                                 `json:"entrypoint"`
	Mounts               []devcontainer.Mount                   `json:"mounts"`
	ContainerEnv         map[string]string                      `json:"containerEnv"`
	RemoteEnv            devcontainer.RemoteEnvironment         `json:"remoteEnv"`
	ContainerUser        string                                 `json:"containerUser"`
	RemoteUser           string                                 `json:"remoteUser"`
	UpdateRemoteUserUID  *bool                                  `json:"updateRemoteUserUID"`
	UserEnvProbe         string                                 `json:"userEnvProbe"`
	OnCreateCommand      devcontainer.Command                   `json:"onCreateCommand"`
	UpdateContentCommand devcontainer.Command                   `json:"updateContentCommand"`
	PostCreateCommand    devcontainer.Command                   `json:"postCreateCommand"`
	PostStartCommand     devcontainer.Command                   `json:"postStartCommand"`
	PostAttachCommand    devcontainer.Command                   `json:"postAttachCommand"`
	WaitFor              devcontainer.LifecycleStage            `json:"waitFor"`
	ShutdownAction       string                                 `json:"shutdownAction"`
	Customizations       map[string]json.RawMessage             `json:"customizations"`
	ForwardPorts         []devcontainer.Port                    `json:"forwardPorts"`
	PortsAttributes      map[string]devcontainer.PortAttributes `json:"portsAttributes"`
	OtherPortsAttributes *devcontainer.PortAttributes           `json:"otherPortsAttributes"`
	Init                 bool                                   `json:"init"`
	Privileged           bool                                   `json:"privileged"`
	CapAdd               []string                               `json:"capAdd"`
	SecurityOpt          []string                               `json:"securityOpt"`
	OverrideCommand      *bool                                  `json:"overrideCommand"`
	HostRequirements     devcontainer.HostRequirements          `json:"hostRequirements"`
}

func (m imageMetadataEntry) configuration() devcontainer.Configuration {
	return devcontainer.Configuration{
		Mounts:               m.Mounts,
		ContainerEnv:         m.ContainerEnv,
		RemoteEnv:            m.RemoteEnv,
		ContainerUser:        m.ContainerUser,
		RemoteUser:           m.RemoteUser,
		UpdateRemoteUserUID:  m.UpdateRemoteUserUID,
		UserEnvProbe:         m.UserEnvProbe,
		OnCreateCommand:      m.OnCreateCommand,
		UpdateContentCommand: m.UpdateContentCommand,
		PostCreateCommand:    m.PostCreateCommand,
		PostStartCommand:     m.PostStartCommand,
		PostAttachCommand:    m.PostAttachCommand,
		WaitFor:              m.WaitFor,
		ShutdownAction:       m.ShutdownAction,
		Customizations:       m.Customizations,
		ForwardPorts:         m.ForwardPorts,
		PortsAttributes:      m.PortsAttributes,
		OtherPortsAttributes: m.OtherPortsAttributes,
		Init:                 m.Init,
		Privileged:           m.Privileged,
		CapAdd:               m.CapAdd,
		SecurityOpt:          m.SecurityOpt,
		OverrideCommand:      m.OverrideCommand,
		HostRequirements:     m.HostRequirements,
	}
}

func (e *Engine) readImageMetadata(ctx context.Context, imageName string) (imageMetadata, error) {
	inspect, err := e.inspectImage(ctx, imageName)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("inspect Dev Container image metadata: %w", err)
	}
	if inspect.Config == nil || inspect.Config.Labels == nil || strings.TrimSpace(inspect.Config.Labels[labelMetadata]) == "" {
		return imageMetadata{}, nil
	}
	metadata, err := parseImageMetadata([]byte(inspect.Config.Labels[labelMetadata]))
	if err != nil {
		return imageMetadata{}, fmt.Errorf("decode image devcontainer.metadata: %w", err)
	}
	return metadata, nil
}

func (e *Engine) imagePlatform() string {
	if e.platform == nil {
		return ""
	}
	platform := strings.TrimSpace(e.platform.OS) + "/" + strings.TrimSpace(e.platform.Architecture)
	if variant := strings.TrimSpace(e.platform.Variant); variant != "" {
		platform += "/" + variant
	}
	return platform
}

func (e *Engine) matchesImagePlatform(os, architecture, variant string) bool {
	return e.platform != nil && os == e.platform.OS && architecture == e.platform.Architecture && (e.platform.Variant == "" || variant == e.platform.Variant)
}

func (e *Engine) availableImageManifestID(manifests []image.ManifestSummary) string {
	for _, manifest := range manifests {
		if manifest.Available && manifest.Kind == image.ManifestKindImage && manifest.ImageData != nil {
			platform := manifest.ImageData.Platform
			if e.matchesImagePlatform(platform.OS, platform.Architecture, platform.Variant) {
				return manifest.ID
			}
		}
	}
	return ""
}

func (e *Engine) localImageAvailable(ctx context.Context, imageName string) (bool, error) {
	inspect, err := e.client.ImageInspect(ctx, imageName, client.ImageInspectWithManifests(true))
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect local image %s: %w", imageName, err)
	}
	if len(inspect.Manifests) > 0 {
		return e.availableImageManifestID(inspect.Manifests) != "", nil
	}
	return inspect.Config != nil && e.matchesImagePlatform(inspect.Os, inspect.Architecture, inspect.Variant), nil
}

func (e *Engine) inspectImage(ctx context.Context, imageName string) (image.InspectResponse, error) {
	inspect, err := e.client.ImageInspect(ctx, imageName, client.ImageInspectWithManifests(true))
	if err != nil || inspect.Config != nil && inspect.Os != "" && inspect.Architecture != "" {
		return inspect, err
	}
	if e.platform == nil {
		return image.InspectResponse{}, fmt.Errorf("inspect image %s: docker server platform is unavailable", imageName)
	}
	if manifestID := e.availableImageManifestID(inspect.Manifests); manifestID != "" {
		resolved, err := e.client.ImageInspect(ctx, manifestID)
		if err != nil {
			return image.InspectResponse{}, fmt.Errorf("inspect %s image manifest %s: %w", imageName, manifestID, err)
		}
		if resolved.Config == nil {
			return image.InspectResponse{}, fmt.Errorf("inspect %s image manifest %s: configuration is missing", imageName, manifestID)
		}
		return resolved, nil
	}
	return image.InspectResponse{}, fmt.Errorf("image %s does not provide an available %s configuration", imageName, e.imagePlatform())
}

func parseImageMetadata(raw []byte) (imageMetadata, error) {
	var values []imageMetadataEntry
	if err := json.Unmarshal(raw, &values); err != nil {
		var value imageMetadataEntry
		if objectErr := json.Unmarshal(raw, &value); objectErr != nil {
			return imageMetadata{}, err
		}
		values = []imageMetadataEntry{value}
	}
	var metadata imageMetadata
	for _, value := range values {
		configuration := value.configuration()
		metadata.Sources = append(metadata.Sources, configuration)
		metadata.Configuration = devcontainer.Merge(metadata.Configuration, configuration)
		if entrypoint := strings.TrimSpace(value.Entrypoint); entrypoint != "" {
			metadata.Entrypoints = append(metadata.Entrypoints, entrypoint)
		}
	}
	return metadata, nil
}

func (e *Engine) prepareUserImage(ctx context.Context, baseImage string, resolved *devcontainer.ResolvedConfiguration, hostUser devcontainer.HostUser) (string, error) {
	if resolved.UpdateRemoteUserUID != nil && !*resolved.UpdateRemoteUserUID || hostUser.UID == 0 || hostUser.GID == 0 || resolved.RemoteUser == "root" || resolved.RemoteUser == "0" {
		return baseImage, nil
	}
	directory, err := os.MkdirTemp("", "devcontainer-user-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	dockerfile := fmt.Sprintf("FROM %s\nUSER root\nCOPY update-user.sh /tmp/update-user.sh\nRUN /bin/sh /tmp/update-user.sh %s %d %d && rm /tmp/update-user.sh\nUSER %s\n", baseImage, shellQuote(resolved.RemoteUser), hostUser.UID, hostUser.GID, resolved.ContainerUser)
	if err := os.WriteFile(filepath.Join(directory, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(directory, "update-user.sh"), []byte(updateUserScript), 0o600); err != nil {
		return "", err
	}
	imageName := "devcontainer-user:" + resolved.DevContainerID
	if err := e.buildImage(ctx, directory, "Dockerfile", imageName, "", nil, nil, nil, resolved.Cache, "remote-user", nil); err != nil {
		return "", fmt.Errorf("build Dev Container user image: %w", err)
	}
	return imageName, nil
}

func (e *Engine) customizeImage(ctx context.Context, baseImage string, resolved *devcontainer.ResolvedConfiguration, customize CustomizeImageFunc, platformEnvironment map[string]string) (string, error) {
	if customize == nil {
		return baseImage, nil
	}
	directory, err := os.MkdirTemp("", "devcontainer-customization-*")
	if err != nil {
		return "", fmt.Errorf("create image customization context: %w", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	containerUser := strings.TrimSpace(resolved.ContainerUser)
	if containerUser == "" {
		containerUser = "root"
	}
	if err := customize(ImageCustomization{
		ContextDir:    directory,
		BaseImage:     baseImage,
		ContainerUser: containerUser,
	}); err != nil {
		return "", fmt.Errorf("customize Dev Container image: %w", err)
	}
	dockerfile, err := os.Stat(filepath.Join(directory, "Dockerfile"))
	if err != nil {
		return "", fmt.Errorf("inspect customized Dev Container Dockerfile: %w", err)
	}
	if !dockerfile.Mode().IsRegular() {
		return "", fmt.Errorf("customized Dev Container Dockerfile is not a regular file")
	}
	imageName := "devcontainer-customized:" + resolved.DevContainerID
	if err := e.buildImage(ctx, directory, "Dockerfile", imageName, "", nil, nil, nil, resolved.Cache, "customization", platformEnvironment); err != nil {
		return "", fmt.Errorf("build customized Dev Container image: %w", err)
	}
	return imageName, nil
}
