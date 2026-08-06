// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	"github.com/docker/cli/cli/command"
	imagecommand "github.com/docker/cli/cli/command/image"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/image"

	"gitea.dev/codespace/devcontainer"
)

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
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("must be an absolute http or https URL")
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

func buildCacheReference(cache devcontainer.CacheOptions, stage string) string {
	return cacheReference(cache, "build", stage, "cache")
}

func imageArtifactCacheReference(cache devcontainer.CacheOptions, stage string, parts ...string) string {
	return cacheReference(cache, append([]string{"image", stage}, parts...)...)
}

func cacheReference(cache devcontainer.CacheOptions, parts ...string) string {
	if strings.TrimSpace(cache.BuildRegistry) == "" || strings.TrimSpace(cache.BuildScope) == "" {
		return ""
	}
	base, err := parseOCIRepositoryBase(cache.BuildRegistry, true)
	if err != nil {
		return ""
	}
	digestInput := strings.Builder{}
	digestInput.WriteString(cache.BuildScope)
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
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	ordered := make(map[string]string, len(values))
	for _, name := range names {
		ordered[name] = values[name]
	}
	content, err := json.Marshal(ordered)
	if err != nil {
		return "{}"
	}
	return string(content)
}

func (e *Engine) useCachedImage(ctx context.Context, imageName, stage string) bool {
	if strings.TrimSpace(imageName) == "" {
		return false
	}
	if err := e.pullImageReference(ctx, imageName); err != nil {
		_, _ = fmt.Fprintf(e.stderr, "Dev Container %s image cache miss: %v\n", stage, err)
		return false
	}
	_, _ = fmt.Fprintf(e.stdout, "Dev Container %s image cache hit\n", stage)
	return true
}

func (e *Engine) publishCachedImage(ctx context.Context, sourceImage, targetImage, stage string) {
	if strings.TrimSpace(targetImage) == "" {
		return
	}
	if err := e.client.ImageTag(ctx, sourceImage, targetImage); err != nil {
		_, _ = fmt.Fprintf(e.stderr, "Warning: tag %s image cache: %v\n", stage, err)
		return
	}
	registryAuth, err := command.RetrieveAuthTokenFromImage(e.cli.ConfigFile(), targetImage)
	if err != nil {
		_, _ = fmt.Fprintf(e.stderr, "Warning: resolve %s image cache credentials: %v\n", stage, err)
		return
	}
	reader, err := e.client.ImagePush(ctx, targetImage, image.PushOptions{RegistryAuth: registryAuth})
	if err != nil {
		_, _ = fmt.Fprintf(e.stderr, "Warning: push %s image cache: %v\n", stage, err)
		return
	}
	defer func() { _ = reader.Close() }()
	if err := streamDockerProgress(reader); err != nil {
		_, _ = fmt.Fprintf(e.stderr, "Warning: publish %s image cache: %v\n", stage, err)
		return
	}
	_, _ = fmt.Fprintf(e.stdout, "Dev Container %s image cache published\n", stage)
}

func streamDockerProgress(reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	for {
		var message dockerProgressMessage
		if err := decoder.Decode(&message); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := message.err(); err != nil {
			return err
		}
	}
}

func (e *Engine) buildImage(ctx context.Context, contextPath, dockerfile, imageName, target string, args map[string]*string, cacheFrom, options []string, cache devcontainer.CacheOptions, stage string) error {
	if len(options) > 0 {
		arguments := []string{"--file", dockerfile, "--tag", imageName}
		if target != "" {
			arguments = append(arguments, "--target", target)
		}
		names := make([]string, 0, len(args))
		for name := range args {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value := name
			if args[name] != nil {
				value += "=" + *args[name]
			}
			arguments = append(arguments, "--build-arg", value)
		}
		for _, source := range cacheFrom {
			arguments = append(arguments, "--cache-from", source)
		}
		arguments = append(arguments, options...)
		arguments = append(arguments, contextPath)
		command := imagecommand.NewBuildCommand(e.cli) //nolint:staticcheck // Docker CLI keeps BuildKit cache flags aligned with user-facing docker build behavior.
		command.SetArgs(arguments)
		command.SetContext(ctx)
		command.SilenceUsage = true
		command.SilenceErrors = true
		return command.Execute()
	}
	buildArgs := make(types.MappingWithEquals, len(args))
	for name, value := range args {
		buildArgs[name] = value
	}
	projectDigest := sha256.Sum256([]byte(imageName))
	service := types.ServiceConfig{
		Name:  "image",
		Image: imageName,
		Build: &types.BuildConfig{Context: contextPath, Dockerfile: dockerfile, Args: buildArgs, Target: target, CacheFrom: types.StringList(cacheFrom)},
	}
	cacheReference := buildCacheReference(cache, stage)
	project := &types.Project{
		Name:       fmt.Sprintf("devcontainer-build-%x", projectDigest[:8]),
		WorkingDir: contextPath,
		Services:   types.Services{"image": service},
	}
	return e.buildService(ctx, project, "image", cacheReference, stage)
}

func (e *Engine) buildService(ctx context.Context, project *types.Project, serviceName, cacheReference, stage string) error {
	service, ok := project.Services[serviceName]
	if !ok || service.Build == nil {
		return nil
	}
	originalCacheFrom := append(types.StringList(nil), service.Build.CacheFrom...)
	originalCacheTo := append(types.StringList(nil), service.Build.CacheTo...)
	if cacheReference != "" {
		service.Build.CacheFrom = append(service.Build.CacheFrom, "type=registry,ref="+cacheReference)
		service.Build.CacheTo = append(service.Build.CacheTo, "type=registry,ref="+cacheReference+",mode=max,oci-mediatypes=true,image-manifest=true,ignore-error=true")
		project.Services[serviceName] = service
		_, _ = fmt.Fprintf(e.stdout, "##[group]Restore and publish %s build cache\n", stage)
	}
	err := e.compose.Build(ctx, project, api.BuildOptions{Services: []string{serviceName}, Progress: "plain", Out: e.stderr})
	if cacheReference != "" {
		_, _ = fmt.Fprintln(e.stdout, "##[endgroup]")
	}
	if err == nil || cacheReference == "" {
		return err
	}
	_, _ = fmt.Fprintf(e.stderr, "Warning: %s BuildKit registry cache is unavailable, retrying with the local cache: %v\n", stage, err)
	service.Build.CacheFrom = originalCacheFrom
	service.Build.CacheTo = originalCacheTo
	project.Services[serviceName] = service
	_, _ = fmt.Fprintf(e.stdout, "##[group]Retry %s build with local cache\n", stage)
	err = e.compose.Build(ctx, project, api.BuildOptions{Services: []string{serviceName}, Progress: "plain", Out: e.stderr})
	_, _ = fmt.Fprintln(e.stdout, "##[endgroup]")
	return err
}
