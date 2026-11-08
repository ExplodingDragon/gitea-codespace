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

func TestComposeProjectNameSeparatesOwnerIDs(t *testing.T) {
	t.Parallel()
	if composeProjectName("owner-a") == composeProjectName("ownera") {
		t.Fatal("distinct owner IDs produced the same Compose project name")
	}
}

func TestApplyPlatformDevicesToComposeService(t *testing.T) {
	t.Parallel()

	service := composetypes.ServiceConfig{
		Gpus: []composetypes.DeviceRequest{{Count: -1}},
		Deploy: &composetypes.DeployConfig{Resources: composetypes.Resources{Reservations: &composetypes.Resource{Devices: []composetypes.DeviceRequest{
			{Count: -1, Driver: "nvidia", Capabilities: []string{"gpu"}},
			{Count: 1, Driver: "example", Capabilities: []string{"accelerator"}},
		}}}},
	}
	devices := PlatformDevices{
		CDI:          []string{"example.com/device=codespace-0-0"},
		Mappings:     []DeviceMapping{{Source: "/dev/example", Target: "/dev/example", Permissions: "rwm"}},
		Capabilities: []string{"SYS_PTRACE"}, ProvidesGPU: true,
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
