// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"context"
	"errors"
	"io"
	"time"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
)

// Apply executes one validated lifecycle request against the local Docker daemon.
func Apply(ctx context.Context, request Request, stdout, stderr io.Writer) (_ *devcontainer.State, returnErr error) {
	if err := request.Validate(); err != nil {
		return nil, err
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
			return nil, devcontainer.InvalidConfiguration(err)
		}
		options.PrepareLifecycle = func(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
			return configureCreate(ctx, engine, state, request)
		}
		state, err := engine.Create(ctx, options)
		if err != nil {
			return nil, err
		}
		if err := startWorkspaceServices(ctx, engine, state, request.Secrets, request.DevContainer, true, stdout, stderr); err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			_ = engine.Delete(cleanup, state)
			return nil, err
		}
		return state, nil
	case "resume":
		state, err := engine.Start(ctx, request.Environment, request.Secrets)
		if err != nil {
			return nil, err
		}
		if err := startWorkspaceServices(ctx, engine, state, request.Secrets, request.DevContainer, false, stdout, stderr); err != nil {
			return nil, err
		}
		return state, nil
	case "stop":
		return engine.Stop(ctx, request.Environment)
	default:
		return engine.Inspect(ctx, request.Environment)
	}
}
