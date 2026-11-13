// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	composeapi "github.com/docker/compose/v2/pkg/api"

	"gitea.dev/codespace/devcontainer"
)

type recordingBuildCompose struct {
	composeapi.Compose
	builds []composetypes.BuildConfig
	err    error
}

func (c *recordingBuildCompose) Build(_ context.Context, project *composetypes.Project, _ composeapi.BuildOptions) error {
	build := *project.Services["image"].Build
	build.Args = maps.Clone(build.Args)
	build.CacheFrom = slices.Clone(build.CacheFrom)
	build.CacheTo = slices.Clone(build.CacheTo)
	c.builds = append(c.builds, build)
	return c.err
}

func TestComposeProjectNameSeparatesOwnerIDs(t *testing.T) {
	t.Parallel()
	if composeProjectName("owner-a") == composeProjectName("ownera") {
		t.Fatal("distinct owner IDs produced the same Compose project name")
	}
}

func TestApplyPlatformDevicesToComposeService(t *testing.T) {
	t.Parallel()

	service := composetypes.ServiceConfig{
		CapAdd: []string{"SYS_PTRACE"},
		Gpus:   []composetypes.DeviceRequest{{Count: -1}},
		Deploy: &composetypes.DeployConfig{Resources: composetypes.Resources{Reservations: &composetypes.Resource{Devices: []composetypes.DeviceRequest{
			{Count: -1, Driver: "nvidia", Capabilities: []string{"gpu"}},
			{Count: 1, Driver: "example", Capabilities: []string{"accelerator"}},
		}}}},
	}
	devices := PlatformDevices{
		CDI:          []string{"example.com/device=codespace-0-0"},
		Mappings:     []DeviceMapping{{Source: "/dev/example", Target: "/dev/example", Permissions: "rwm"}},
		Capabilities: []string{"SYS_PTRACE", "SYS_RAWIO"}, ProvidesGPU: true,
	}
	if err := applyPlatformDevicesToComposeService(&service, devices); err != nil {
		t.Fatal(err)
	}
	if len(service.Gpus) != 0 || len(service.Deploy.Resources.Reservations.Devices) != 1 || service.Deploy.Resources.Reservations.Devices[0].Driver != "example" {
		t.Fatalf("normalized Compose GPU requests = %#v / %#v", service.Gpus, service.Deploy.Resources.Reservations.Devices)
	}
	if len(service.Devices) != 2 || service.Devices[0].Source != devices.CDI[0] || service.Devices[0].Target != devices.CDI[0] || !slices.Equal(service.CapAdd, devices.Capabilities) {
		t.Fatalf("Compose platform devices = %#v / %#v", service.Devices, service.CapAdd)
	}
	service.Gpus = []composetypes.DeviceRequest{{Count: 1, Driver: "nvidia", Capabilities: []string{"gpu"}}}
	if err := applyPlatformDevicesToComposeService(&service, devices); err == nil {
		t.Fatal("specific Compose GPU selection must not override the Kubernetes allocation")
	}
}

func TestComposeProxyEnvironment(t *testing.T) {
	t.Parallel()

	composeFile, err := filepath.Abs(filepath.Join("testdata", "official-interop", ".devcontainer", "compose.yaml"))
	if err != nil {
		t.Fatalf("resolve Compose fixture: %v", err)
	}
	options, err := composecli.NewProjectOptions([]string{composeFile},
		composecli.WithName("proxy-environment-test"),
		composecli.WithWorkingDirectory(filepath.Dir(composeFile)),
		composecli.WithResolvedPaths(true),
	)
	if err != nil {
		t.Fatalf("prepare Compose fixture: %v", err)
	}
	project, err := options.LoadProject(t.Context())
	if err != nil {
		t.Fatalf("load Compose fixture: %v", err)
	}
	workspace := project.Services["workspace"]
	workspace.Networks = maps.Clone(workspace.Networks)
	if workspace.Networks == nil {
		workspace.Networks = map[string]*composetypes.ServiceNetworkConfig{}
	}
	workspace.Networks["internal"] = &composetypes.ServiceNetworkConfig{Aliases: []string{"workspace-alias"}}
	project.Services["workspace"] = workspace

	withoutProxy := map[string]string{"NO_PROXY": "registry.internal"}
	if result := composeProxyEnvironment(project.Services, withoutProxy); !maps.Equal(result, withoutProxy) {
		t.Fatalf("environment without a proxy = %#v", result)
	}
	withProxy := composeProxyEnvironment(project.Services, map[string]string{
		"HTTPS_PROXY": "http://proxy.example:3128",
		"NO_PROXY":    "registry.internal",
	})
	if withProxy["NO_PROXY"] != "sidecar,workspace,workspace-alias,registry.internal" || withProxy["no_proxy"] != withProxy["NO_PROXY"] {
		t.Fatalf("Compose proxy environment = %#v", withProxy)
	}
}

func TestBuildServiceCacheOptions(t *testing.T) {
	t.Parallel()

	cache := devcontainer.CacheOptions{BuildRegistry: "https://registry.example.com/codespace", BuildScope: "scope"}
	for _, test := range []struct {
		name  string
		cache devcontainer.CacheOptions
		err   error
	}{
		{name: "registry cache", cache: cache},
		{name: "build failure", cache: cache, err: errors.New("RUN install.sh failed")},
		{name: "canceled build", cache: cache, err: context.Canceled},
		{name: "user cache only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			compose := &recordingBuildCompose{err: test.err}
			engine := &Engine{compose: compose, stdout: io.Discard, stderr: io.Discard}
			userArgument := "user-value"
			original := &composetypes.BuildConfig{
				Args:      composetypes.MappingWithEquals{"USER_ARGUMENT": &userArgument},
				CacheFrom: composetypes.StringList{"type=local,src=/cache/import"},
				CacheTo:   composetypes.StringList{"type=local,dest=/cache/export"},
			}
			project := &composetypes.Project{Services: composetypes.Services{"image": {Name: "image", Build: original}}}
			err := engine.buildService(t.Context(), project, "image", test.cache, "repository", map[string]string{"HTTPS_PROXY": "http://proxy.example:3128"})
			if !errors.Is(err, test.err) || len(compose.builds) != 1 {
				t.Fatalf("build result = %v, calls = %d", err, len(compose.builds))
			}
			build := compose.builds[0]
			wantFrom, wantTo := slices.Clone(original.CacheFrom), slices.Clone(original.CacheTo)
			if source, destination := registryBuildCache(test.cache, "repository"); source != "" {
				wantFrom, wantTo = append(wantFrom, source), append(wantTo, destination)
			}
			if !slices.Equal(build.CacheFrom, wantFrom) || !slices.Equal(build.CacheTo, wantTo) {
				t.Fatalf("build cache = %#v / %#v", build.CacheFrom, build.CacheTo)
			}
			if build.Args["HTTPS_PROXY"] == nil || *build.Args["HTTPS_PROXY"] != "http://proxy.example:3128" || build.Args["USER_ARGUMENT"] != &userArgument {
				t.Fatalf("build arguments = %#v", build.Args)
			}
			if project.Services["image"].Build != original || len(original.Args) != 1 || len(original.CacheFrom) != 1 || len(original.CacheTo) != 1 {
				t.Fatalf("project build configuration changed: %#v", original)
			}
		})
	}
}
