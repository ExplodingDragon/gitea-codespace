// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
)

// Apply executes one validated lifecycle request against the local Docker daemon.
func Apply(ctx context.Context, request Request, stdout, stderr io.Writer) (_ *devcontainer.State, returnErr error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	engine, err := containerdocker.New(ctx, stdout, stderr)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, engine.Close()) }()
	switch request.Action {
	case "create":
		options, err := buildCreateOptions(request)
		if err != nil {
			return nil, err
		}
		options.CustomizeImage = imageCustomization(request.DevContainer, platformGiteaCLIPath, stdout)
		options.PrepareLifecycle = func(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
			if err := runTemplateLifecycle(ctx, request, engine, state, "start"); err != nil {
				return err
			}
			if err := configureCreate(ctx, engine, state, request); err != nil {
				return err
			}
			_, _, err := ConfiguredEndpoints(state)
			return err
		}
		state, err := engine.Create(ctx, options)
		if err != nil {
			return nil, err
		}
		startWebIDE(ctx, engine, state, request.Secrets, request.DevContainer, true, stdout, stderr)
		return state, nil
	case "resume":
		startFailed := false
		state, err := engine.Start(ctx, request.Environment, request.HostUser, request.LocalEnvironment, request.Secrets, func(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
			if err := runTemplateLifecycle(ctx, request, engine, state, "start"); err != nil {
				startFailed = true
				return err
			}
			if err := configureCA(ctx, engine, state, request.GiteaCustomCA); err != nil {
				return err
			}
			values := map[string]string{
				"GITEA_CONFIGURE_CA_ONLY": "true",
				"GITEA_URL":               request.GiteaURL,
			}
			if request.GiteaCustomCA {
				values["GITEA_CA_BUNDLE"] = containerCABundlePath
			}
			if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/sh", "-c", configureGitScript}, values, nil); err != nil {
				return fmt.Errorf("configure Gitea CA for Git in Dev Container: %w", err)
			}
			return nil
		}, request.RunLifecycle)
		if err != nil {
			if startFailed {
				stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
				defer cancel()
				_, stopErr := engine.Stop(stopContext, request.Environment)
				return nil, errors.Join(err, stopErr)
			}
			return nil, err
		}
		startWebIDE(ctx, engine, state, request.Secrets, request.DevContainer, false, stdout, stderr)
		return state, nil
	case "stop":
		lifecycleErr := runTemplateLifecycle(ctx, request, engine, request.Environment, "stop")
		stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		state, stopErr := engine.Stop(stopContext, request.Environment)
		return state, errors.Join(lifecycleErr, stopErr)
	case "delete":
		return engine.Stop(ctx, request.Environment)
	default:
		return engine.Inspect(ctx, request.Environment)
	}
}

func runTemplateLifecycle(ctx context.Context, request Request, engine *containerdocker.Engine, state *devcontainer.State, action string) error {
	if strings.TrimSpace(request.DevContainer.LifecycleScript) == "" {
		return nil
	}
	if action != "start" && action != "stop" {
		return fmt.Errorf("invalid environment-template lifecycle action %q", action)
	}
	run := func(ctx context.Context) error {
		if action == "stop" {
			running, err := engine.ContainerRunning(ctx, state.PrimaryContainerID)
			if cerrdefs.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("inspect Dev Container before stop lifecycle: %w", err)
			}
			if !running {
				return nil
			}
		}
		lifecycleContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := engine.Run(lifecycleContext, state.PrimaryContainerID, "0", state.RemoteWorkdir, []string{"/bin/sh", templateLifecycleScriptPath, action}, nil, nil); err != nil {
			return fmt.Errorf("run environment-template lifecycle %s: %w", action, err)
		}
		return nil
	}
	if request.RunTemplateLifecycle != nil {
		return request.RunTemplateLifecycle(ctx, action, run)
	}
	return run(ctx)
}
