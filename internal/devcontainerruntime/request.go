// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"errors"
	"fmt"
	"strings"

	"gitea.dev/codespace/devcontainer"
)

// FormatVersion identifies the runtime request and result representation.
const FormatVersion = 2

// Request carries one lifecycle action from the provisioner into the instance runtime.
type Request struct {
	Version           int                       `json:"version"`
	Action            string                    `json:"action"`
	CodespaceUUID     string                    `json:"codespace_uuid"`
	OperationVersion  int64                     `json:"operation_version"`
	Workspace         string                    `json:"workspace"`
	Source            devcontainer.Source       `json:"source"`
	HostUser          devcontainer.HostUser     `json:"host_user"`
	GitUserName       string                    `json:"git_user_name,omitempty"`
	GitUserEmail      string                    `json:"git_user_email,omitempty"`
	LocalEnvironment  map[string]string         `json:"local_environment,omitempty"`
	Secrets           map[string]string         `json:"secrets,omitempty"`
	CodeServerVersion string                    `json:"code_server_version,omitempty"`
	Cache             devcontainer.CacheOptions `json:"cache,omitempty"`
	Environment       *devcontainer.State       `json:"environment,omitempty"`
}

// Result records either the resulting environment state or a classified failure.
type Result struct {
	Version     int                 `json:"version"`
	Environment *devcontainer.State `json:"environment,omitempty"`
	Error       string              `json:"error,omitempty"`
	Recoverable bool                `json:"recoverable,omitempty"`
}

// Validate checks action-specific identity and state requirements.
func (r Request) Validate() error {
	if r.Version != FormatVersion {
		return formatError("runtime request version must be %d", FormatVersion)
	}
	if strings.TrimSpace(r.CodespaceUUID) == "" {
		return formatError("codespace uuid is empty")
	}
	switch r.Action {
	case "create":
		if r.OperationVersion <= 0 || strings.TrimSpace(r.Workspace) == "" {
			return formatError("create request identity is incomplete")
		}
		if strings.TrimSpace(r.CodeServerVersion) == "" {
			return formatError("create request code-server version is empty")
		}
		if r.Environment != nil {
			return formatError("create request contains an existing environment")
		}
	case "resume", "stop", "inspect":
		if r.Action != "inspect" && r.OperationVersion <= 0 {
			return formatError("operation version must be positive")
		}
		if r.Environment == nil {
			return formatError("environment is required for %s", r.Action)
		}
		if err := r.Environment.Validate(); err != nil {
			return formatError("%v", err)
		}
		if r.Environment.OwnerID != r.CodespaceUUID {
			return formatError("runtime environment does not belong to codespace")
		}
	default:
		return formatError("runtime action %q is invalid", r.Action)
	}
	return nil
}

type runtimeFormatError string

func (e runtimeFormatError) Error() string { return string(e) }

// IsFormatError reports whether a request cannot be processed without changing its encoded input.
func IsFormatError(err error) bool {
	var target runtimeFormatError
	return errors.As(err, &target)
}

func formatError(format string, values ...any) error {
	return runtimeFormatError(fmt.Sprintf(format, values...))
}
