// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/image"

	"gitea.dev/codespace/devcontainer"
)

const imageTransferIdleTimeout = 2 * time.Minute

// Cache transfers can be large; only time without progress triggers fallback.
func watchImageTransfer(parent context.Context) (context.Context, func(), func(error) error) {
	ctx, cancel := context.WithCancelCause(parent)
	timer := time.AfterFunc(imageTransferIdleTimeout, func() {
		cancel(errors.New("OCI transfer made no progress for " + imageTransferIdleTimeout.String()))
	})
	return ctx, func() { timer.Reset(imageTransferIdleTimeout) }, func(err error) error {
		timer.Stop()
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		cancel(nil)
		return err
	}
}

func validateCacheOptions(cache devcontainer.CacheOptions) error {
	if cache.BuildRegistry != "" {
		if _, err := parseOCIRepositoryBase(cache.BuildRegistry, true); err != nil {
			return fmt.Errorf("buildkit cache registry: %w", err)
		}
		if strings.TrimSpace(cache.BuildScope) == "" {
			return fmt.Errorf("buildkit cache scope is empty")
		}
	}
	for registry, mirror := range cache.Mirrors {
		if registry == "" || registry != strings.ToLower(strings.TrimSpace(registry)) || strings.Contains(registry, "://") {
			return fmt.Errorf("oci mirror registry %q is invalid", registry)
		}
		if _, err := parseOCIRepositoryBase(mirror, false); err != nil {
			return fmt.Errorf("oci mirror for %s: %w", registry, err)
		}
	}
	for registry, credential := range cache.Credentials {
		if strings.TrimSpace(registry) == "" {
			return fmt.Errorf("oci cache credential registry is empty")
		}
		if strings.TrimSpace(credential.Username) == "" || strings.TrimSpace(credential.Password) == "" {
			return fmt.Errorf("oci cache credential for %s is incomplete", registry)
		}
	}
	return nil
}

func parseOCIRepositoryBase(value string, requirePath bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("must not contain credentials, query, or fragment")
	}
	if requirePath && strings.Trim(parsed.Path, "/") == "" {
		return nil, fmt.Errorf("must include a namespace path")
	}
	repository := parsed.Host
	if namespace := strings.Trim(parsed.Path, "/"); namespace != "" {
		repository += "/" + namespace
	}
	if _, err := reference.WithName(repository); err != nil {
		return nil, fmt.Errorf("must contain a valid OCI registry host and namespace: %w", err)
	}
	return parsed, nil
}

func mirroredReference(value string, mirrors map[string]string) (string, bool, error) {
	named, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return "", false, err
	}
	mirror, ok := mirrors[reference.Domain(named)]
	if !ok {
		return value, false, nil
	}
	base, err := parseOCIRepositoryBase(mirror, false)
	if err != nil {
		return "", false, err
	}
	repository := base.Host + "/" + path.Join(strings.Trim(base.Path, "/"), reference.Path(named))
	if digested, ok := named.(reference.Digested); ok {
		return repository + "@" + digested.Digest().String(), true, nil
	}
	named = reference.TagNameOnly(named)
	return repository + ":" + named.(reference.Tagged).Tag(), true, nil
}

func registryBuildCache(cache devcontainer.CacheOptions, stage, platform string) (string, string) {
	reference := cacheReference(cache, "build", cache.BuildScope, stage, platform)
	if reference == "" {
		return "", ""
	}
	return "type=registry,ref=" + reference,
		"type=registry,ref=" + reference + ",mode=max,oci-mediatypes=true,image-manifest=true,ignore-error=true"
}

func cacheReference(cache devcontainer.CacheOptions, parts ...string) string {
	if strings.TrimSpace(cache.BuildRegistry) == "" {
		return ""
	}
	base, err := parseOCIRepositoryBase(cache.BuildRegistry, true)
	if err != nil {
		return ""
	}
	digestInput := strings.Builder{}
	for _, part := range parts {
		digestInput.WriteByte(0)
		digestInput.WriteString(part)
	}
	digest := sha256.Sum256([]byte(digestInput.String()))
	return base.Host + "/" + path.Join(strings.Trim(base.Path, "/"), fmt.Sprintf("%x", digest[:])) + ":cache"
}

func stringMapCachePart(values map[string]string) string {
	if len(values) == 0 {
		return "{}"
	}
	content, _ := json.Marshal(values)
	return string(content)
}

func (e *Engine) useCachedImage(ctx context.Context, imageName, stage string) (bool, error) {
	if strings.TrimSpace(imageName) == "" {
		return false, ctx.Err()
	}
	started := time.Now()
	_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Checking %s image cache\n", stage)
	available, err := e.localImageAvailable(ctx, imageName)
	if err != nil {
		return false, err
	}
	if available {
		_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Using %s image cache (local)\n", stage)
		return true, nil
	}
	transferCtx, progress, finish := watchImageTransfer(ctx)
	err = finish(e.pullImageReference(transferCtx, imageName, progress))
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] %s image cache not found after %s; building locally\n", stage, time.Since(started).Round(time.Millisecond))
		} else {
			_, _ = fmt.Fprintf(e.stderr, "[WARN] [build] %s image cache unavailable after %s; building locally: %v\n", stage, time.Since(started).Round(time.Millisecond), err)
		}
		return false, nil
	}
	_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Using %s image cache (pulled in %s)\n", stage, time.Since(started).Round(time.Millisecond))
	return true, nil
}

func (e *Engine) publishCachedImage(ctx context.Context, sourceImage, targetImage, stage string) {
	if strings.TrimSpace(targetImage) == "" {
		return
	}
	started := time.Now()
	ctx, progress, finish := watchImageTransfer(ctx)
	var err error
	defer func() {
		if err := finish(err); err != nil {
			_, _ = fmt.Fprintf(e.stderr, "[WARN] [build] Cannot publish %s image cache; built image remains usable: %v\n", stage, err)
		} else {
			_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Published %s image cache in %s\n", stage, time.Since(started).Round(time.Millisecond))
		}
	}()
	if err = e.client.ImageTag(ctx, sourceImage, targetImage); err != nil {
		err = fmt.Errorf("tag image: %w", err)
		return
	}
	registryAuth, err := command.RetrieveAuthTokenFromImage(e.cli.ConfigFile(), targetImage)
	if err != nil {
		err = fmt.Errorf("resolve registry credentials: %w", err)
		return
	}
	reader, err := e.client.ImagePush(ctx, targetImage, image.PushOptions{RegistryAuth: registryAuth})
	if err != nil {
		err = fmt.Errorf("push image: %w", err)
		return
	}
	defer func() { _ = reader.Close() }()
	progress()
	err = readDockerProgress(reader, observeImageTransfer(progress))
}

func (e *Engine) buildImage(ctx context.Context, contextPath, dockerfile, imageName, target string, args map[string]*string, cacheFrom, options []string, cache devcontainer.CacheOptions, stage string, platformEnvironment map[string]string) error {
	if len(options) > 0 {
		_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Building %s image\n", stage)
		args = mergeBuildProxyArguments(args, platformEnvironment)
		// Buildx supports registry cache exports and loads the result for container creation.
		arguments := []string{"buildx", "build", "--load", "--progress=plain", "--file", filepath.Join(contextPath, dockerfile), "--tag", imageName}
		if target != "" {
			arguments = append(arguments, "--target", target)
		}
		for _, name := range slices.Sorted(maps.Keys(args)) {
			value := name
			if args[name] != nil {
				value += "=" + *args[name]
			}
			arguments = append(arguments, "--build-arg", value)
		}
		for _, source := range cacheFrom {
			arguments = append(arguments, "--cache-from", source)
		}
		platform := os.Getenv("DOCKER_DEFAULT_PLATFORM")
		if platform == "" {
			platform = e.imagePlatform()
		}
		for index, option := range options {
			if value, ok := strings.CutPrefix(option, "--platform="); ok {
				platform = value
			} else if option == "--platform" && index+1 < len(options) {
				platform = options[index+1]
			}
		}
		if source, destination := registryBuildCache(cache, stage, platform); source != "" {
			arguments = append(arguments, "--cache-from", source, "--cache-to", destination)
			_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Import and export %s build cache (export failures are non-fatal)\n", stage)
		}
		arguments = append(arguments, options...)
		arguments = append(arguments, contextPath)
		command := exec.CommandContext(ctx, "docker", arguments...)
		command.Stdout, command.Stderr = e.stdout, e.stderr
		return command.Run()
	}
	buildArgs := make(types.MappingWithEquals, len(args))
	maps.Copy(buildArgs, args)
	projectDigest := sha256.Sum256([]byte(imageName))
	service := types.ServiceConfig{
		Name:  "image",
		Image: imageName,
		Build: &types.BuildConfig{Context: contextPath, Dockerfile: dockerfile, Args: buildArgs, Target: target, CacheFrom: types.StringList(cacheFrom)},
	}
	project := &types.Project{
		Name:       fmt.Sprintf("devcontainer-build-%x", projectDigest[:8]),
		WorkingDir: contextPath,
		Services:   types.Services{"image": service},
	}
	return e.buildService(ctx, project, "image", cache, stage, platformEnvironment)
}

func (e *Engine) buildService(ctx context.Context, project *types.Project, serviceName string, cache devcontainer.CacheOptions, stage string, platformEnvironment map[string]string) error {
	service, ok := project.Services[serviceName]
	if !ok || service.Build == nil {
		return nil
	}
	_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Building %s image (service %s)\n", stage, serviceName)
	originalBuild := service.Build
	build := *originalBuild
	build.Args = types.MappingWithEquals(mergeBuildProxyArguments(build.Args, platformEnvironment))
	platform := service.Platform
	if len(build.Platforms) > 0 {
		platforms := slices.Clone(build.Platforms)
		slices.Sort(platforms)
		platform = strings.Join(platforms, ",")
	}
	if platform == "" {
		platform = os.Getenv("DOCKER_DEFAULT_PLATFORM")
	}
	if platform == "" {
		platform = e.imagePlatform()
	}
	if source, destination := registryBuildCache(cache, stage, platform); source != "" {
		build.CacheFrom = append(slices.Clone(build.CacheFrom), source)
		build.CacheTo = append(slices.Clone(build.CacheTo), destination)
		_, _ = fmt.Fprintf(e.stdout, "[INFO] [build] Import and export %s build cache (export failures are non-fatal)\n", stage)
	}
	service.Build = &build
	project.Services[serviceName] = service
	defer func() {
		service.Build = originalBuild
		project.Services[serviceName] = service
	}()
	return e.compose.Build(ctx, project, api.BuildOptions{Services: []string{serviceName}, Progress: "plain", Out: e.stderr})
}

func mergeBuildProxyArguments(arguments map[string]*string, environment map[string]string) map[string]*string {
	result := make(map[string]*string, len(arguments)+8)
	maps.Copy(result, arguments)
	for _, name := range devcontainer.ProxyVariableNames() {
		if value := environment[name]; value != "" {
			value := value
			result[name] = &value
		}
	}
	return result
}
