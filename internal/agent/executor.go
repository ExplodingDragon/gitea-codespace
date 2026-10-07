// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/devcontainerruntime"
)

// Executor persists the completed environment before acknowledging a side effect.
type Executor struct {
	Journal *Journal
	PodUID  string
	Stdout  io.Writer
	Stderr  io.Writer
}

func (e *Executor) Apply(ctx context.Context, request devcontainerruntime.Request) (*devcontainer.State, error) {
	if e.Journal == nil || e.PodUID == "" || request.CodespaceUUID != e.Journal.record.Identity.RuntimeUUID {
		return nil, fmt.Errorf("runtime execution identity does not match the volume")
	}
	if request.Action != "create" {
		state, err := e.Environment()
		if err != nil {
			return nil, err
		}
		request.Environment = state
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if request.Action == "inspect" {
		return devcontainerruntime.Apply(ctx, request, e.Stdout, e.Stderr)
	}
	stage := request.Action
	if stage == "resume" {
		stage += "/" + e.PodUID
	}
	err := e.Journal.RunStage(ctx, request.OperationVersion, stage, func(ctx context.Context) error {
		state, err := devcontainerruntime.Apply(ctx, request, e.Stdout, e.Stderr)
		if err != nil {
			return err
		}
		if err := state.Validate(); err != nil {
			return err
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if len(data) > 4*1024*1024 {
			return fmt.Errorf("dev container recovery state exceeds the size limit")
		}
		// Request.Secrets and cache credentials remain in memory, outside State.
		return e.Journal.write("state/environment.json", data)
	})
	if err != nil {
		return nil, err
	}
	return e.Environment()
}

func (e *Executor) Environment() (*devcontainer.State, error) {
	data, err := e.Journal.read("state/environment.json", 4*1024*1024)
	if err != nil {
		return nil, fmt.Errorf("read saved Dev Container: %w", err)
	}
	state := &devcontainer.State{}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("decode saved Dev Container: %w", err)
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	if state.OwnerID != e.Journal.record.Identity.RuntimeUUID {
		return nil, fmt.Errorf("saved Dev Container belongs to another Codespace")
	}
	return state, nil
}
