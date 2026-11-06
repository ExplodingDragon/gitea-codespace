// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/container"

	"gitea.dev/codespace/devcontainer"
)

func (e *Engine) initializeContainer(ctx context.Context, environment *devcontainer.State, secrets map[string]string, startLifecycle, prepareLifecycle PrepareLifecycleFunc, runLifecycle RunLifecycleFunc, ready ReadyFunc) error {
	inspect, err := e.client.ContainerInspect(ctx, environment.PrimaryContainerID)
	if err != nil {
		return fmt.Errorf("inspect Dev Container: %w", err)
	}
	if environment.RemoteUser == "" {
		environment.RemoteUser = strings.TrimSpace(inspect.Config.User)
		if environment.RemoteUser == "" {
			environment.RemoteUser = "root"
		}
	}
	remoteEnv, err := e.probeRemoteEnvironment(ctx, environment)
	if err != nil {
		return err
	}
	for name, value := range environment.Configuration.RemoteEnv {
		if value == nil {
			delete(remoteEnv, name)
		} else {
			remoteEnv[name] = *value
		}
	}
	environment.RemoteEnvironment = remoteEnv
	if startLifecycle != nil {
		if err := startLifecycle(ctx, e, environment); err != nil {
			return fmt.Errorf("run environment-template start lifecycle: %w", err)
		}
	}
	if prepareLifecycle != nil {
		if err := prepareLifecycle(ctx, e, environment); err != nil {
			return fmt.Errorf("prepare Dev Container lifecycle: %w", err)
		}
	}
	readySent := false
	emitReady := func(stage devcontainer.LifecycleStage) {
		if !readySent && environment.Configuration.WaitFor == stage {
			readySent = true
			if ready != nil {
				ready(environment, stage)
			}
		}
	}
	emitReady(devcontainer.LifecycleStageInitialize)
	commands := []struct {
		name     devcontainer.LifecycleStage
		commands []devcontainer.Command
	}{
		{name: devcontainer.LifecycleStageOnCreate, commands: environment.LifecycleCommands.OnCreate},
		{name: devcontainer.LifecycleStageUpdateContent, commands: environment.LifecycleCommands.UpdateContent},
		{name: devcontainer.LifecycleStagePostCreate, commands: environment.LifecycleCommands.PostCreate},
		{name: devcontainer.LifecycleStagePostStart, commands: environment.LifecycleCommands.PostStart},
	}
	for _, item := range commands {
		if err := e.executeLifecycleStage(ctx, environment, item.name, item.commands, secrets, runLifecycle); err != nil {
			return err
		}
		emitReady(item.name)
	}
	return nil
}

// Start resumes an existing environment and runs postStartCommand.
func (e *Engine) Start(ctx context.Context, environment *devcontainer.State, hostUser devcontainer.HostUser, localEnvironment, secrets map[string]string, startLifecycle, prepare PrepareLifecycleFunc, runLifecycle RunLifecycleFunc) (*devcontainer.State, error) {
	if err := environment.Validate(); err != nil {
		return nil, err
	}
	if err := runLifecycleStage(ctx, runLifecycle, devcontainer.LifecycleStageInitialize, environment.Configuration.InitializeCommand, func(ctx context.Context) error {
		return runInitializeCommand(ctx, environment.Configuration.InitializeCommand, hostUser, environment.Workspace, localEnvironment, e.stdout, e.stderr)
	}); err != nil {
		return nil, fmt.Errorf("run initializeCommand: %w", err)
	}
	if environment.ComposeProject != "" {
		if err := e.compose.Start(ctx, environment.ComposeProject, api.StartOptions{Wait: true, WaitTimeout: 2 * time.Minute}); err != nil {
			return nil, fmt.Errorf("resume Docker Compose Dev Container: %w", err)
		}
	} else {
		ids := append(slices.Clone(environment.RelatedContainerIDs), environment.PrimaryContainerID)
		for _, id := range ids {
			inspect, err := e.client.ContainerInspect(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("inspect Dev Container %s: %w", id, err)
			}
			if !inspect.State.Running {
				if err := e.client.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
					return nil, fmt.Errorf("resume Dev Container %s: %w", id, err)
				}
			}
		}
	}
	if startLifecycle != nil {
		if err := startLifecycle(ctx, e, environment); err != nil {
			return nil, fmt.Errorf("run environment-template start lifecycle: %w", err)
		}
	}
	if prepare != nil {
		if err := prepare(ctx, e, environment); err != nil {
			return nil, err
		}
	}
	if err := e.executeLifecycleStage(ctx, environment, devcontainer.LifecycleStagePostStart, environment.LifecycleCommands.PostStart, secrets, runLifecycle); err != nil {
		return nil, err
	}
	return e.Inspect(ctx, environment)
}

// RunTemplateLifecycle executes the environment-template script in the primary container.
func (e *Engine) RunTemplateLifecycle(ctx context.Context, environment *devcontainer.State, action string) error {
	if action != "start" && action != "stop" {
		return fmt.Errorf("invalid environment-template lifecycle action %q", action)
	}
	if action == "stop" {
		inspect, err := e.client.ContainerInspect(ctx, environment.PrimaryContainerID)
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect Dev Container before stop lifecycle: %w", err)
		}
		if !inspect.State.Running {
			return nil
		}
	}
	lifecycleContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := e.Run(lifecycleContext, environment.PrimaryContainerID, "0", environment.RemoteWorkdir, []string{"/bin/sh", templateLifecycleScriptPath, action}, nil, nil); err != nil {
		return fmt.Errorf("run environment-template lifecycle %s: %w", action, err)
	}
	return nil
}

// Stop stops all resources belonging to an environment without deleting them.
func (e *Engine) Stop(ctx context.Context, environment *devcontainer.State) (*devcontainer.State, error) {
	if err := environment.Validate(); err != nil {
		return nil, err
	}
	timeout := 20 * time.Second
	if environment.ComposeProject != "" {
		if err := e.compose.Stop(ctx, environment.ComposeProject, api.StopOptions{Timeout: &timeout}); err != nil {
			return nil, fmt.Errorf("stop Docker Compose Dev Container: %w", err)
		}
		return environment, nil
	}
	timeoutSeconds := int(timeout.Seconds())
	for _, id := range environmentContainerIDs(environment) {
		if err := e.client.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeoutSeconds}); err != nil && !cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("stop Dev Container %s: %w", id, err)
		}
	}
	return environment, nil
}

// Inspect verifies resource identity and requires the primary container to be running.
func (e *Engine) Inspect(ctx context.Context, environment *devcontainer.State) (*devcontainer.State, error) {
	for _, id := range environmentContainerIDs(environment) {
		inspect, err := e.client.ContainerInspect(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("inspect Dev Container %s: %w", id, err)
		}
		if id == environment.PrimaryContainerID && !inspect.State.Running {
			return nil, fmt.Errorf("dev container %s is not running", id)
		}
		if inspect.Config.Labels[labelOwnerID] != environment.OwnerID || inspect.Config.Labels[labelEnvironmentID] != environment.ID {
			return nil, fmt.Errorf("dev container %s identity does not match runtime state", id)
		}
	}
	return environment, nil
}

// Delete removes all resources belonging to an environment.
func (e *Engine) Delete(ctx context.Context, environment *devcontainer.State) error {
	var result error
	if environment.ComposeProject != "" {
		result = errors.Join(result, e.downComposeProject(ctx, environment.ComposeProject))
	}
	for _, id := range environmentContainerIDs(environment) {
		if id == "" {
			continue
		}
		if err := e.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func environmentContainerIDs(environment *devcontainer.State) []string {
	return append([]string{environment.PrimaryContainerID}, environment.RelatedContainerIDs...)
}
