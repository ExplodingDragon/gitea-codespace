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

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/image"
	dockerunits "github.com/docker/go-units"

	"gitea.dev/codespace/devcontainer"
)

//go:embed builtin/update-user.sh
var updateUserScript string

func (e *Engine) resolveImage(ctx context.Context, resolved *devcontainer.ResolvedConfiguration) (string, map[string]string, error) {
	var baseImage string
	if resolved.Image != "" {
		var err error
		baseImage, err = e.resolveAndPullImage(ctx, resolved.Image, resolved.Cache)
		if err != nil {
			return "", nil, err
		}
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
		contextPath, err := resolvePathInsideRoot(resolved.AllowedPathRoot, contextPath)
		if err != nil {
			return "", nil, fmt.Errorf("resolve Dev Container build context: %w", err)
		}
		dockerfile := buildConfig.Dockerfile
		if !filepath.IsAbs(dockerfile) {
			dockerfile = filepath.Join(resolved.ConfigurationDir, dockerfile)
		}
		dockerfile, err = resolvePathInsideRoot(resolved.AllowedPathRoot, dockerfile)
		if err != nil {
			return "", nil, fmt.Errorf("resolve Dev Container Dockerfile: %w", err)
		}
		relativeDockerfile, err := filepath.Rel(contextPath, dockerfile)
		if err != nil || relativeDockerfile == ".." || strings.HasPrefix(relativeDockerfile, ".."+string(filepath.Separator)) {
			return "", nil, fmt.Errorf("Dev Container Dockerfile must be inside its build context")
		}
		imageName := "devcontainer-build:" + resolved.DevContainerID
		args := make(map[string]*string, len(buildConfig.Args))
		for name, value := range buildConfig.Args {
			args[name] = &value
		}
		if err := e.buildImage(ctx, contextPath, relativeDockerfile, imageName, buildConfig.Target, args, buildConfig.CacheFrom, resolved.Cache, "repository"); err != nil {
			return "", nil, fmt.Errorf("build Dev Container image: %w", err)
		}
		baseImage = imageName
	}
	repositoryConfiguration := resolved.Configuration
	metadata, err := e.readImageMetadata(ctx, baseImage)
	if err != nil {
		return "", nil, err
	}
	imageConfiguration := metadata.Configuration
	resolved.FeatureEntrypoints = append(resolved.FeatureEntrypoints, metadata.Entrypoints...)
	resolved.Configuration = devcontainer.Merge(imageConfiguration, repositoryConfiguration)
	return e.applyFeatures(ctx, baseImage, resolved, imageConfiguration, repositoryConfiguration)
}

func (e *Engine) resolveAndPullImage(ctx context.Context, imageName string, cache devcontainer.CacheOptions) (string, error) {
	fetchReference, mirrored, err := mirroredReference(imageName, cache.Mirrors)
	if err != nil {
		return "", fmt.Errorf("resolve OCI mirror for %s: %w", imageName, err)
	}
	if mirrored {
		if err := e.pullImageReference(ctx, fetchReference); err == nil {
			fmt.Fprintf(e.stderr, "Pulled image %s through mirror %s\n", imageName, fetchReference)
			return fetchReference, nil
		} else {
			fmt.Fprintf(e.stderr, "Warning: OCI mirror for %s is unavailable, falling back to the original registry: %v\n", imageName, err)
		}
	}
	if err := e.pullImageReference(ctx, imageName); err != nil {
		return "", err
	}
	return imageName, nil
}

func (e *Engine) pullImageReference(ctx context.Context, imageName string) error {
	registryAuth, err := command.RetrieveAuthTokenFromImage(e.cli.ConfigFile(), imageName)
	if err != nil {
		return fmt.Errorf("resolve registry credentials for %s: %w", imageName, err)
	}
	reader, err := e.client.ImagePull(ctx, imageName, image.PullOptions{RegistryAuth: registryAuth})
	if err != nil {
		return fmt.Errorf("pull Dev Container image %s: %w", imageName, err)
	}
	defer reader.Close()
	fmt.Fprintf(e.stderr, "Pulling image %s\n", imageName)
	if err := streamDockerPullProgress(reader, e.stderr); err != nil {
		return fmt.Errorf("read image pull progress: %w", err)
	}
	fmt.Fprintf(e.stderr, "Pulled image %s\n", imageName)
	return nil
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

func streamDockerPullProgress(reader io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(reader)
	progress := dockerPullProgress{}
	for {
		var message dockerProgressMessage
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := message.err(); err != nil {
			return err
		}
		if summary := progress.observe(message, time.Now()); summary != "" {
			fmt.Fprintln(output, summary)
		}
	}
}

type imageMetadata struct {
	Configuration devcontainer.Configuration
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
	inspect, err := e.client.ImageInspect(ctx, imageName)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("inspect Dev Container image metadata: %w", err)
	}
	if inspect.Config == nil || inspect.Config.Labels == nil || strings.TrimSpace(inspect.Config.Labels[labelMetadata]) == "" {
		return imageMetadata{}, nil
	}
	metadata, err := parseImageMetadata([]byte(inspect.Config.Labels[labelMetadata]))
	if err != nil {
		return imageMetadata{}, devcontainer.InvalidConfiguration(fmt.Errorf("decode image devcontainer.metadata: %w", err))
	}
	return metadata, nil
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
		metadata.Configuration = devcontainer.Merge(metadata.Configuration, value.configuration())
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
	defer os.RemoveAll(directory)
	dockerfile := fmt.Sprintf("FROM %s\nUSER root\nCOPY update-user.sh /tmp/update-user.sh\nRUN /bin/sh /tmp/update-user.sh %s %d %d && rm /tmp/update-user.sh\nUSER %s\n", baseImage, shellQuote(resolved.RemoteUser), hostUser.UID, hostUser.GID, resolved.ContainerUser)
	if err := os.WriteFile(filepath.Join(directory, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(directory, "update-user.sh"), []byte(updateUserScript), 0o600); err != nil {
		return "", err
	}
	imageName := "devcontainer-user:" + resolved.DevContainerID
	if err := e.buildImage(ctx, directory, "Dockerfile", imageName, "", nil, nil, resolved.Cache, "remote-user"); err != nil {
		return "", fmt.Errorf("build Dev Container user image: %w", err)
	}
	return imageName, nil
}
