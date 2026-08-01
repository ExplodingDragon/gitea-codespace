// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	imagecommand "github.com/docker/cli/cli/command/image"
	"github.com/docker/compose/v2/pkg/api"

	"gitea.dev/codespace/devcontainer"
)

func validateCacheOptions(cache devcontainer.CacheOptions) error {
	if cache.BuildRegistry != "" {
		if _, err := parseOCIRepositoryBase(cache.BuildRegistry, true); err != nil {
			return fmt.Errorf("BuildKit cache registry: %w", err)
		}
		if strings.TrimSpace(cache.BuildScope) == "" {
			return fmt.Errorf("BuildKit cache scope is empty")
		}
	}
	for registry, mirror := range cache.Mirrors {
		if registry == "" || registry != strings.ToLower(strings.TrimSpace(registry)) || strings.Contains(registry, "://") {
			return fmt.Errorf("OCI mirror registry %q is invalid", registry)
		}
		if _, err := parseOCIRepositoryBase(mirror, false); err != nil {
			return fmt.Errorf("OCI mirror for %s: %w", registry, err)
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
	if strings.TrimSpace(cache.BuildRegistry) == "" || strings.TrimSpace(cache.BuildScope) == "" {
		return ""
	}
	base, err := parseOCIRepositoryBase(cache.BuildRegistry, true)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256([]byte(cache.BuildScope + "\x00" + stage))
	return base.Host + "/" + path.Join(strings.Trim(base.Path, "/"), fmt.Sprintf("%x", digest[:])) + ":cache"
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
		command := imagecommand.NewBuildCommand(e.cli)
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
		fmt.Fprintf(e.stdout, "##[group]Restore and publish %s build cache\n", stage)
	}
	err := e.compose.Build(ctx, project, api.BuildOptions{Services: []string{serviceName}, Progress: "plain", Out: e.stderr})
	if cacheReference != "" {
		fmt.Fprintln(e.stdout, "##[endgroup]")
	}
	if err == nil || cacheReference == "" {
		return err
	}
	fmt.Fprintf(e.stderr, "Warning: %s BuildKit registry cache is unavailable, retrying with the local cache: %v\n", stage, err)
	service.Build.CacheFrom = originalCacheFrom
	service.Build.CacheTo = originalCacheTo
	project.Services[serviceName] = service
	fmt.Fprintf(e.stdout, "##[group]Retry %s build with local cache\n", stage)
	err = e.compose.Build(ctx, project, api.BuildOptions{Services: []string{serviceName}, Progress: "plain", Out: e.stderr})
	fmt.Fprintln(e.stdout, "##[endgroup]")
	return err
}
