// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	composeapi "github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"

	"gitea.dev/codespace/devcontainer"
)

func TestDevContainerIDIsStableAndAlphanumeric(t *testing.T) {
	t.Parallel()

	first := devContainerID("codespace-1")
	if first != devContainerID("codespace-1") || first == devContainerID("codespace-2") || len(first) != 64 {
		t.Fatalf("Dev Container IDs are not stable and unique: %q", first)
	}
	for _, char := range first {
		if char < '0' || char > '9' && char < 'a' || char > 'f' {
			t.Fatalf("Dev Container ID contains %q", char)
		}
	}
}

type cacheFallbackCompose struct {
	composeapi.Compose
	cacheFrom []composetypes.StringList
	cacheTo   []composetypes.StringList
}

func (c *cacheFallbackCompose) Build(_ context.Context, project *composetypes.Project, _ composeapi.BuildOptions) error {
	service := project.Services["image"]
	c.cacheFrom = append(c.cacheFrom, slices.Clone(service.Build.CacheFrom))
	c.cacheTo = append(c.cacheTo, slices.Clone(service.Build.CacheTo))
	if len(c.cacheFrom) == 1 {
		return errors.New("cache manifest is absent")
	}
	return nil
}

func TestEngineConfigurationHelpers(t *testing.T) {
	t.Parallel()

	mounts, err := devContainerMounts("/host/workspace", "/workspaces/project", "", []devcontainer.Mount{
		{Type: "volume", Source: "replacement", Target: "/workspaces/project", VolumeNoCopy: true},
		{Type: "bind", Source: "/host/cache", Target: "/cache", BindPropagation: "rshared"},
	})
	if err != nil {
		t.Fatalf("resolve mounts: %v", err)
	}
	if len(mounts) != 2 || mounts[0].Source != "replacement" || !mounts[0].VolumeNoCopy || mounts[1].BindPropagation != "rshared" {
		t.Fatalf("resolved mounts = %#v", mounts)
	}
	if composeProjectName("owner-a") == composeProjectName("ownera") {
		t.Fatal("distinct owner IDs produced the same Compose project name")
	}
	publishSpecs, err := appPortPublishSpecs(devcontainer.AppPortList{
		{Number: 8000, Numeric: true},
		{Address: "127.0.0.1:9000:90"},
	})
	if err != nil {
		t.Fatalf("resolve appPort publish specs: %v", err)
	}
	if !slices.Equal(publishSpecs, []string{"127.0.0.1:8000:8000", "127.0.0.1:9000:90"}) {
		t.Fatalf("appPort publish specs = %#v", publishSpecs)
	}
}

func TestPlatformProxyEnvironmentDoesNotMutateInputs(t *testing.T) {
	t.Parallel()
	base := map[string]string{"NO_PROXY": "repository.internal", "VALUE": "repository"}
	platform := map[string]string{"HTTP_PROXY": "http://proxy.example:3128", "NO_PROXY": ".svc,repository.internal"}
	merged := mergePlatformEnvironment(base, platform)
	if merged["HTTP_PROXY"] != platform["HTTP_PROXY"] || merged["NO_PROXY"] != "repository.internal,.svc" || merged["no_proxy"] != merged["NO_PROXY"] {
		t.Fatalf("merged platform environment = %#v", merged)
	}
	if len(base) != 2 || len(platform) != 2 {
		t.Fatalf("proxy merge mutated its inputs: %#v / %#v", base, platform)
	}
	build := mergeBuildProxyArguments(map[string]*string{"VERSION": nil}, platform)
	if build["HTTP_PROXY"] == nil || *build["HTTP_PROXY"] != platform["HTTP_PROXY"] || build["VERSION"] != nil {
		t.Fatalf("build proxy arguments = %#v", build)
	}
}

func TestDockerCreateArgumentsWithRunArgsPreserveFeatureStartup(t *testing.T) {
	t.Parallel()

	resolved := &devcontainer.ResolvedConfiguration{
		DevContainerID: "environment-1",
		Configuration:  devcontainer.Configuration{RunArgs: []string{"--hostname", "workspace"}},
	}
	config := &container.Config{
		Image:        "example/image:latest",
		WorkingDir:   "/workspace",
		User:         "codespace",
		Env:          []string{"A=B"},
		Labels:       map[string]string{"z": "last", "a": "first"},
		Entrypoint:   []string{"/bin/sh"},
		Cmd:          []string{"-c", "feature-entrypoint\nexec \"$@\"", "-", "/start.sh"},
		ExposedPorts: nil,
	}
	hostConfig := &container.HostConfig{
		Mounts:     []mount.Mount{{Type: mount.TypeBind, Source: "/host/workspace", Target: "/workspace"}},
		Privileged: true,
	}
	arguments, err := dockerCreateArguments(resolved, config, hostConfig, []string{"127.0.0.1:8000:8000"})
	if err != nil {
		t.Fatalf("create Docker arguments: %v", err)
	}
	for _, required := range [][]string{
		{"--publish", "127.0.0.1:8000:8000"},
		{"--hostname", "workspace"},
		{"--entrypoint", "/bin/sh"},
		{"example/image:latest", "-c"},
	} {
		if !containsAdjacent(arguments, required[0], required[1]) {
			t.Fatalf("Docker arguments missing %q %q: %#v", required[0], required[1], arguments)
		}
	}
	imageIndex := slices.Index(arguments, "example/image:latest")
	if imageIndex < 0 || imageIndex+4 >= len(arguments) || arguments[imageIndex+1] != "-c" || arguments[imageIndex+3] != "-" || arguments[imageIndex+4] != "/start.sh" {
		t.Fatalf("Docker command does not preserve startup wrapper after image: %#v", arguments)
	}
}

func TestDockerCreateArgumentsRejectEntrypointRunArg(t *testing.T) {
	t.Parallel()

	_, err := dockerCreateArguments(&devcontainer.ResolvedConfiguration{
		Configuration: devcontainer.Configuration{RunArgs: []string{"--entrypoint=/custom"}},
	}, &container.Config{Image: "example/image"}, &container.HostConfig{}, nil)
	if err == nil || !strings.Contains(err.Error(), "Feature startup entrypoints") {
		t.Fatalf("entrypoint runArg error = %v", err)
	}
}

func containsAdjacent(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}

func TestCacheReferences(t *testing.T) {
	t.Parallel()

	mirrors := map[string]string{
		"docker.io": "https://cache.example.com/docker",
		"ghcr.io":   "https://cache.example.com/ghcr",
	}
	image, mirrored, err := mirroredReference("ubuntu:24.04", mirrors)
	if err != nil || !mirrored || image != "cache.example.com/docker/library/ubuntu:24.04" {
		t.Fatalf("Docker Hub mirror = %q, %v, %v", image, mirrored, err)
	}
	digest := strings.Repeat("a", 64)
	image, mirrored, err = mirroredReference("ghcr.io/example/tool@sha256:"+digest, mirrors)
	if err != nil || !mirrored || image != "cache.example.com/ghcr/example/tool@sha256:"+digest {
		t.Fatalf("GHCR mirror = %q, %v, %v", image, mirrored, err)
	}
	cache := devcontainer.CacheOptions{BuildRegistry: "https://registry.example.com/codespace", BuildScope: "scope"}
	first := buildCacheReference(cache, "features")
	if first == "" || first == buildCacheReference(cache, "remote-user") || first != buildCacheReference(cache, "features") {
		t.Fatalf("BuildKit cache references are not stable and stage-specific: %q", first)
	}
	featureImage := featureImageArtifactCacheReference(cache, "sha256:base", []*resolvedFeature{{
		reference: "ghcr.io/devcontainers/features/git:1",
		digest:    "sha256:feature",
		options:   map[string]string{"version": "latest"},
	}}, "root", "root", map[string]string{"PATH": "/usr/bin"})
	if featureImage == "" || featureImage == first {
		t.Fatalf("Feature image cache reference = %q", featureImage)
	}
	changedFeature := featureImageArtifactCacheReference(cache, "sha256:base", []*resolvedFeature{{
		reference: "ghcr.io/devcontainers/features/git:1",
		digest:    "sha256:changed",
		options:   map[string]string{"version": "latest"},
	}}, "root", "root", map[string]string{"PATH": "/usr/bin"})
	if featureImage == changedFeature || featureImage != featureImageArtifactCacheReference(cache, "sha256:base", []*resolvedFeature{{
		reference: "ghcr.io/devcontainers/features/git:1",
		digest:    "sha256:feature",
		options:   map[string]string{"version": "latest"},
	}}, "root", "root", map[string]string{"PATH": "/usr/bin"}) {
		t.Fatalf("Feature image cache reference is not stable and content-specific: %q / %q", featureImage, changedFeature)
	}
}

func TestBuildCacheFallbackPreservesLocalCache(t *testing.T) {
	t.Parallel()

	compose := &cacheFallbackCompose{}
	engine := &Engine{compose: compose, stdout: io.Discard, stderr: io.Discard}
	project := &composetypes.Project{Services: composetypes.Services{"image": {
		Name: "image",
		Build: &composetypes.BuildConfig{
			CacheFrom: composetypes.StringList{"type=local,src=/cache/import"},
			CacheTo:   composetypes.StringList{"type=local,dest=/cache/export"},
		},
	}}}
	cache := devcontainer.CacheOptions{BuildRegistry: "https://registry.example.com/codespace", BuildScope: "scope"}
	reference := buildCacheReference(cache, "repository")
	if err := engine.buildService(t.Context(), project, "image", cache, "repository", nil); err != nil {
		t.Fatal(err)
	}
	registryImport := "type=registry,ref=" + reference
	if len(compose.cacheFrom) != 2 || !slices.Contains(compose.cacheFrom[0], registryImport) || slices.Contains(compose.cacheFrom[1], registryImport) {
		t.Fatalf("cache import attempts = %#v", compose.cacheFrom)
	}
	registryExport := "type=registry,ref=" + reference + ",mode=max,oci-mediatypes=true,image-manifest=true"
	if len(compose.cacheTo) != 2 || !slices.Contains(compose.cacheTo[0], registryExport) || slices.Contains(compose.cacheTo[1], registryExport) || !slices.Contains(compose.cacheTo[1], "type=local,dest=/cache/export") {
		t.Fatalf("cache export attempts = %#v", compose.cacheTo)
	}
}

func TestMergeInjectedFeaturesKeepsDistinctFeatureVersions(t *testing.T) {
	t.Parallel()

	resolved := &devcontainer.ResolvedConfiguration{Configuration: devcontainer.Configuration{Features: map[string]json.RawMessage{
		"ghcr.io/example/features/tool:1": json.RawMessage(`{"version":"1"}`),
	}}}
	if err := mergeInjectedFeatures(resolved, []devcontainer.InjectedFeature{{
		Reference: "ghcr.io/example/features/tool:2",
		Origin:    "user",
		Options:   map[string]json.RawMessage{"channel": json.RawMessage(`"stable"`)},
	}}); err != nil {
		t.Fatalf("override repository Feature: %v", err)
	}
	if _, exists := resolved.Features["ghcr.io/example/features/tool:1"]; !exists {
		t.Fatal("repository Feature is missing")
	}
	want := `{"channel":"stable"}`
	got := resolved.Features["ghcr.io/example/features/tool:2"]
	var gotOptions, wantOptions map[string]any
	if json.Unmarshal(got, &gotOptions) != nil || json.Unmarshal([]byte(want), &wantOptions) != nil || !maps.Equal(gotOptions, wantOptions) {
		t.Fatalf("merged Feature options = %s, want %s", got, want)
	}
	if _, exists := resolved.PlatformFeatureReferences["ghcr.io/example/features/tool:2"]; !exists {
		t.Fatal("platform-only Feature is not excluded from the repository lockfile")
	}
}

func TestMergeInjectedFeaturesKeepsRepositoryLockOwnership(t *testing.T) {
	t.Parallel()

	const repositoryReference = "ghcr.io/example/features/tool:1"
	resolved := &devcontainer.ResolvedConfiguration{Configuration: devcontainer.Configuration{Features: map[string]json.RawMessage{
		repositoryReference: json.RawMessage(`{"version":"1"}`),
	}}}
	if err := mergeInjectedFeatures(resolved, []devcontainer.InjectedFeature{{
		Reference: "GHCR.IO/EXAMPLE/FEATURES/TOOL:1",
		Origin:    "environment template",
		Options:   map[string]json.RawMessage{"channel": json.RawMessage(`"stable"`)},
	}}); err != nil {
		t.Fatalf("merge platform Feature: %v", err)
	}
	if _, excluded := resolved.PlatformFeatureReferences[repositoryReference]; excluded {
		t.Fatal("repository Feature was excluded from its lockfile after platform options were merged")
	}
	if len(resolved.Features) != 1 {
		t.Fatalf("merged Features = %#v", resolved.Features)
	}
	var options map[string]any
	if err := json.Unmarshal(resolved.Features[repositoryReference], &options); err != nil {
		t.Fatalf("decode merged Feature: %v", err)
	}
	if options["version"] != "1" || options["channel"] != "stable" {
		t.Fatalf("merged Feature options = %#v", options)
	}
}

func TestMergeInjectedFeaturesRecordsInstallOnlyPolicy(t *testing.T) {
	t.Parallel()

	const reference = "ghcr.io/example/features/editor:1"
	resolved := &devcontainer.ResolvedConfiguration{Configuration: devcontainer.Configuration{Features: map[string]json.RawMessage{
		reference: json.RawMessage(`{"version":"1"}`),
	}}}
	if err := mergeInjectedFeatures(resolved, []devcontainer.InjectedFeature{{
		Reference:   reference,
		Origin:      "platform",
		Options:     map[string]json.RawMessage{"version": json.RawMessage(`"1"`)},
		InstallOnly: true,
	}}); err != nil {
		t.Fatalf("merge matching install-only Feature: %v", err)
	}
	if _, ok := resolved.InstallOnlyFeatures[reference]; !ok {
		t.Fatalf("install-only Feature policy = %#v", resolved.InstallOnlyFeatures)
	}
}
