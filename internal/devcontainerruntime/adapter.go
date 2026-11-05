// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"gitea.dev/codespace/internal/runtimeendpoint"
)

//go:embed builtin/configure-git.sh
var configureGitScript string

//go:embed builtin/install-ca.sh
var installCAScript string

//go:embed builtin/start-web-ide.sh
var startWebIDEScript string

//go:embed builtin/endpoint.sh
var endpointScript []byte

const (
	// ContainerRuntimeBinary is the in-container path used by endpoint and TCP bridges.
	ContainerRuntimeBinary  = "/usr/local/libexec/gitea-codespace-runtime"
	containerEndpointBinary = "/usr/local/bin/gitea-codespace-endpoint"
	containerCABundlePath   = "/var/lib/gitea-codespace/ca/bundle.pem"
)

var runtimeMounts = [...]struct {
	source   string
	target   string
	readOnly bool
}{
	{source: "/run/codespace/gitea-token", target: "/var/lib/gitea-codespace/gitea-token", readOnly: true},
	{source: "/run/codespace/git", target: "/var/lib/gitea-codespace/git", readOnly: true},
	{source: "/run/codespace/ca", target: "/var/lib/gitea-codespace/ca", readOnly: true},
	{source: runtimeendpoint.AgentSocketPath, target: runtimeendpoint.ContainerSocketPath},
	{source: "/usr/local/bin/gitea-codespace", target: ContainerRuntimeBinary, readOnly: true},
}

func buildCreateOptions(request Request) (containerdocker.CreateOptions, error) {
	mounts := make([]devcontainer.Mount, 0, len(runtimeMounts))
	for _, item := range runtimeMounts {
		if _, err := os.Stat(item.source); err == nil {
			mounts = append(mounts, devcontainer.Mount{Type: "bind", Source: item.source, Target: item.target, ReadOnly: item.readOnly})
		} else if !os.IsNotExist(err) {
			return containerdocker.CreateOptions{}, fmt.Errorf("inspect runtime mount %s: %w", item.source, err)
		}
	}
	injected := make([]devcontainer.InjectedFeature, 0, len(request.DevContainer.Features)+1)
	for _, feature := range request.DevContainer.Features {
		options := map[string]json.RawMessage{}
		if len(feature.Options.Raw) != 0 {
			if err := json.Unmarshal(feature.Options.Raw, &options); err != nil {
				return containerdocker.CreateOptions{}, fmt.Errorf("decode Dev Container Feature %s options: %w", feature.Reference, err)
			}
		}
		injected = append(injected, devcontainer.InjectedFeature{Reference: strings.TrimSpace(feature.Reference), Origin: "environment template", Options: options})
	}
	if webIDE := request.DevContainer.WebIDE; webIDE.Enabled {
		injected = append(injected, devcontainer.InjectedFeature{
			Reference: webIDEFeatureReference, Origin: "platform Web IDE", InstallOnly: true,
			Options: map[string]json.RawMessage{
				"version":            json.RawMessage(strconv.Quote(strings.TrimSpace(webIDE.Version))),
				"auth":               json.RawMessage(`"none"`),
				"host":               json.RawMessage(`"0.0.0.0"`),
				"port":               json.RawMessage(strconv.Quote(fmt.Sprint(runtimeendpoint.WorkspaceEndpointPort))),
				"disableTelemetry":   json.RawMessage("true"),
				"disableUpdateCheck": json.RawMessage("true"),
			},
		})
	}
	containerEnvironment := map[string]string{
		"CODESPACE_CA_BUNDLE": containerCABundlePath,
		"NODE_EXTRA_CA_CERTS": containerCABundlePath,
	}
	platformEnvironment := map[string]string{}
	for _, name := range devcontainer.ProxyVariableNames() {
		if value := request.LocalEnvironment[name]; value != "" {
			platformEnvironment[name] = value
		}
	}
	if request.GiteaCustomCA {
		containerEnvironment["GITEA_CA_BUNDLE"] = containerCABundlePath
	}
	return containerdocker.CreateOptions{
		OwnerID:             request.CodespaceUUID,
		Workspace:           request.Workspace,
		Source:              request.Source,
		AllowedPathRoot:     request.Workspace,
		HostUser:            request.HostUser,
		LocalEnvironment:    request.LocalEnvironment,
		PlatformEnvironment: platformEnvironment,
		Secrets:             request.Secrets,
		Cache:               request.Cache,
		RunLifecycle:        request.RunLifecycle,
		InjectedFeatures:    injected,
		AdditionalMounts:    mounts,
		ContainerEnv:        containerEnvironment,
		Labels:              map[string]string{"dev.gitea.codespace.uuid": request.CodespaceUUID},
	}, nil
}

func configureCreate(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, request Request) error {
	if err := configureCA(ctx, engine, state, request.GiteaCustomCA); err != nil {
		return err
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, "0", "/", []string{"mkdir", "-p", "/usr/local/libexec", "/usr/local/bin"}, nil, nil); err != nil {
		return fmt.Errorf("prepare platform directories in Dev Container: %w", err)
	}
	if err := engine.CopyContent(ctx, state.PrimaryContainerID, containerEndpointBinary, 0o755, endpointScript); err != nil {
		return err
	}
	values := map[string]string{
		"GITEA_URL":            request.GiteaURL,
		"GITEA_GIT_USER_NAME":  request.GitUserName,
		"GITEA_GIT_USER_EMAIL": request.GitUserEmail,
		"GITEA_WORKSPACE":      state.RemoteWorkdir,
	}
	if request.GiteaCustomCA {
		values["GITEA_CA_BUNDLE"] = containerCABundlePath
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/sh", "-c", configureGitScript}, values, nil); err != nil {
		return fmt.Errorf("configure Git in Dev Container: %w", err)
	}
	return nil
}

func configureCA(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, giteaCustom bool) error {
	if err := engine.Run(ctx, state.PrimaryContainerID, "0", "/", []string{"/bin/sh", "-c", installCAScript}, nil, nil); err != nil {
		return fmt.Errorf("configure platform CA in Dev Container: %w", err)
	}
	if state.RemoteEnvironment == nil {
		state.RemoteEnvironment = map[string]string{}
	}
	state.RemoteEnvironment["CODESPACE_CA_BUNDLE"] = containerCABundlePath
	state.RemoteEnvironment["NODE_EXTRA_CA_CERTS"] = containerCABundlePath
	if giteaCustom {
		state.RemoteEnvironment["GITEA_CA_BUNDLE"] = containerCABundlePath
	} else {
		delete(state.RemoteEnvironment, "GITEA_CA_BUNDLE")
	}
	return nil
}

func configureGitCA(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, giteaURL string, custom bool) error {
	values := map[string]string{
		"GITEA_CONFIGURE_CA_ONLY": "true",
		"GITEA_URL":               giteaURL,
	}
	if custom {
		values["GITEA_CA_BUNDLE"] = containerCABundlePath
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/sh", "-c", configureGitScript}, values, nil); err != nil {
		return fmt.Errorf("configure Gitea CA for Git in Dev Container: %w", err)
	}
	return nil
}

func startWorkspaceServices(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, secrets map[string]string, configuration Configuration, initializeWebIDE bool, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if !configuration.WebIDE.Enabled {
		return nil
	}
	values := devcontainer.ProcessEnvironment(state.RemoteEnvironment, secrets, map[string]string{
		"GITEA_WEB_IDE_INITIALIZE": strconv.FormatBool(initializeWebIDE),
		"GITEA_WEB_IDE_PORT":       fmt.Sprint(runtimeendpoint.WorkspaceEndpointPort),
		"GITEA_WORKSPACE":          state.RemoteWorkdir,
	})
	var settingsReader io.Reader
	customizations := webIDECustomizations{}
	if initializeWebIDE {
		var err error
		customizations, err = resolveWebIDECustomizations(configuration.WebIDE, state.Configuration.Customizations["vscode"])
		if err != nil {
			return err
		}
		settings, err := json.Marshal(customizations.Settings)
		if err != nil {
			return fmt.Errorf("encode Web IDE settings: %w", err)
		}
		settingsReader = bytes.NewReader(settings)
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/sh", "-c", startWebIDEScript}, values, settingsReader); err != nil {
		return fmt.Errorf("start platform Web IDE: %w", err)
	}
	if err := engine.RunPostAttach(ctx, state, secrets); err != nil {
		return err
	}
	if !initializeWebIDE {
		return nil
	}
	_, _ = fmt.Fprintln(stdout, "Install VS Code extensions")
	for _, extension := range customizations.Extensions {
		if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"code-server", "--install-extension", extension}, values, nil); err != nil {
			_, _ = fmt.Fprintf(stderr, "Warning: Install VS Code extension %s: %v\n", extension, err)
		}
	}
	return nil
}

type webIDECustomizations struct {
	Settings   map[string]json.RawMessage `json:"settings"`
	Extensions []string                   `json:"extensions"`
}

func resolveWebIDECustomizations(configuration WebIDEConfiguration, repository json.RawMessage) (webIDECustomizations, error) {
	result := webIDECustomizations{Settings: map[string]json.RawMessage{}}
	if len(configuration.Settings.Raw) != 0 {
		if err := json.Unmarshal(configuration.Settings.Raw, &result.Settings); err != nil {
			return webIDECustomizations{}, fmt.Errorf("decode environment-template Web IDE settings: %w", err)
		}
	}
	repositoryConfiguration := webIDECustomizations{}
	if len(repository) != 0 {
		if err := json.Unmarshal(repository, &repositoryConfiguration); err != nil {
			return webIDECustomizations{}, fmt.Errorf("decode VS Code customizations: %w", err)
		}
	}
	for name, value := range repositoryConfiguration.Settings {
		result.Settings[name] = value
	}

	extensions := make(map[string]string, len(configuration.Extensions)+len(repositoryConfiguration.Extensions))
	ordered := make(map[string]struct{}, len(configuration.Extensions)+len(repositoryConfiguration.Extensions))
	order := make([]string, 0, len(configuration.Extensions)+len(repositoryConfiguration.Extensions))
	apply := func(values []string) error {
		for _, value := range values {
			identity, extension, remove, err := parseWebIDEExtension(value)
			if err != nil {
				return fmt.Errorf("VS Code extension %q: %w", value, err)
			}
			if remove {
				delete(extensions, identity)
				continue
			}
			if _, exists := ordered[identity]; !exists {
				ordered[identity] = struct{}{}
				order = append(order, identity)
			}
			extensions[identity] = extension
		}
		return nil
	}
	if err := apply(configuration.Extensions); err != nil {
		return webIDECustomizations{}, err
	}
	if err := apply(repositoryConfiguration.Extensions); err != nil {
		return webIDECustomizations{}, err
	}
	for _, identity := range order {
		if extension := extensions[identity]; extension != "" {
			result.Extensions = append(result.Extensions, extension)
		}
	}
	return result, nil
}
