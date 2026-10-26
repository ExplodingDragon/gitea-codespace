// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"fmt"
	"strings"

	"gitea.dev/codespace/devcontainer"
)

// Request supplies one Agent lifecycle action to the Dev Container executor.
type Request struct {
	Action           string
	CodespaceUUID    string
	OperationVersion int64
	Workspace        string
	Source           devcontainer.Source
	HostUser         devcontainer.HostUser
	GitUserName      string
	GitUserEmail     string
	LocalEnvironment map[string]string
	Secrets          map[string]string
	DevContainer     Configuration
	Cache            devcontainer.CacheOptions
	Environment      *devcontainer.State
}

// Validate checks action-specific identity and state requirements.
func (r Request) Validate() error {
	if strings.TrimSpace(r.CodespaceUUID) == "" {
		return fmt.Errorf("codespace uuid is empty")
	}
	switch r.Action {
	case "create":
		if r.OperationVersion <= 0 || strings.TrimSpace(r.Workspace) == "" {
			return fmt.Errorf("create request identity is incomplete")
		}
		if err := r.DevContainer.Validate(); err != nil {
			return fmt.Errorf("invalid Dev Container injection configuration: %w", err)
		}
		if r.Environment != nil {
			return fmt.Errorf("create request contains an existing environment")
		}
	case "resume", "stop", "inspect":
		if r.Action != "inspect" && r.OperationVersion <= 0 {
			return fmt.Errorf("operation version must be positive")
		}
		if r.Environment == nil {
			return fmt.Errorf("environment is required for %s", r.Action)
		}
		if err := r.Environment.Validate(); err != nil {
			return err
		}
		if r.Environment.OwnerID != r.CodespaceUUID {
			return fmt.Errorf("runtime environment does not belong to codespace")
		}
	default:
		return fmt.Errorf("runtime action %q is invalid", r.Action)
	}
	return nil
}
