// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/pkg/archive"

	"gitea.dev/codespace/devcontainer"
)

//go:embed builtin/update-user.sh
var updateUserScript string

func (e *Engine) resolveImage(ctx context.Context, resolved *devcontainer.ResolvedConfiguration) (string, map[string]string, error) {
	var baseImage string
	if resolved.Image != "" {
		if err := e.pullImage(ctx, resolved.Image); err != nil {
			return "", nil, err
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
		contextPath, err := resolvePath(resolved.AllowedPathRoot, contextPath)
		if err != nil {
			return "", nil, fmt.Errorf("resolve Dev Container build context: %w", err)
		}
		dockerfile := buildConfig.Dockerfile
		if !filepath.IsAbs(dockerfile) {
			dockerfile = filepath.Join(resolved.ConfigurationDir, dockerfile)
		}
		dockerfile, err = resolvePath(resolved.AllowedPathRoot, dockerfile)
		if err != nil {
			return "", nil, fmt.Errorf("resolve Dev Container Dockerfile: %w", err)
		}
		relativeDockerfile, err := filepath.Rel(contextPath, dockerfile)
		if err != nil || relativeDockerfile == ".." || strings.HasPrefix(relativeDockerfile, ".."+string(filepath.Separator)) {
			return "", nil, fmt.Errorf("Dev Container Dockerfile must be inside its build context")
		}
		reader, err := archive.TarWithOptions(contextPath, &archive.TarOptions{})
		if err != nil {
			return "", nil, fmt.Errorf("archive Dev Container build context: %w", err)
		}
		defer reader.Close()
		imageName := "devcontainer-build:" + resolved.DevContainerID
		args := make(map[string]*string, len(buildConfig.Args))
		for name, value := range buildConfig.Args {
			value := value
			args[name] = &value
		}
		response, err := e.client.ImageBuild(ctx, reader, build.ImageBuildOptions{
			Dockerfile: relativeDockerfile,
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
	repositoryConfiguration := resolved.Configuration
	imageConfiguration, err := e.readImageMetadata(ctx, baseImage)
	if err != nil {
		return "", nil, err
	}
	resolved.Configuration = devcontainer.Merge(imageConfiguration, repositoryConfiguration)
	return e.applyFeatures(ctx, baseImage, resolved, imageConfiguration, repositoryConfiguration)
}

func (e *Engine) pullImage(ctx context.Context, imageName string) error {
	registryAuth, err := command.RetrieveAuthTokenFromImage(e.cli.ConfigFile(), imageName)
	if err != nil {
		return fmt.Errorf("resolve registry credentials for %s: %w", imageName, err)
	}
	reader, err := e.client.ImagePull(ctx, imageName, image.PullOptions{RegistryAuth: registryAuth})
	if err != nil {
		return fmt.Errorf("pull Dev Container image %s: %w", imageName, err)
	}
	defer reader.Close()
	if err := streamDockerProgress(reader, e.stderr); err != nil {
		return fmt.Errorf("read image pull progress: %w", err)
	}
	return nil
}

func (e *Engine) readImageMetadata(ctx context.Context, imageName string) (devcontainer.Configuration, error) {
	inspect, err := e.client.ImageInspect(ctx, imageName)
	if err != nil {
		return devcontainer.Configuration{}, fmt.Errorf("inspect Dev Container image metadata: %w", err)
	}
	if inspect.Config == nil || inspect.Config.Labels == nil || strings.TrimSpace(inspect.Config.Labels[labelMetadata]) == "" {
		return devcontainer.Configuration{}, nil
	}
	raw := []byte(inspect.Config.Labels[labelMetadata])
	var values []devcontainer.Configuration
	if err := json.Unmarshal(raw, &values); err != nil {
		var value devcontainer.Configuration
		if objectErr := json.Unmarshal(raw, &value); objectErr != nil {
			return devcontainer.Configuration{}, devcontainer.InvalidConfiguration(fmt.Errorf("decode image devcontainer.metadata: %w", err))
		}
		values = []devcontainer.Configuration{value}
	}
	var merged devcontainer.Configuration
	for _, value := range values {
		merged = devcontainer.Merge(merged, value)
	}
	return merged, nil
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
	reader, err := archive.TarWithOptions(directory, &archive.TarOptions{})
	if err != nil {
		return "", err
	}
	defer reader.Close()
	imageName := "devcontainer-user:" + resolved.DevContainerID
	response, err := e.client.ImageBuild(ctx, reader, build.ImageBuildOptions{Dockerfile: "Dockerfile", Tags: []string{imageName}, Remove: true})
	if err != nil {
		return "", fmt.Errorf("build Dev Container user image: %w", err)
	}
	defer response.Body.Close()
	if err := streamDockerProgress(response.Body, e.stderr); err != nil {
		return "", fmt.Errorf("build Dev Container user image: %w", err)
	}
	return imageName, nil
}
