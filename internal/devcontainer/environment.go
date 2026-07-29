// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"errors"
	"fmt"
	"strings"
)

const RuntimeFormatVersion = 1

// RuntimeRequest is one exact-version request executed inside an Incus instance.
type RuntimeRequest struct {
	Version          int               `json:"version"`
	Action           string            `json:"action"`
	CodespaceUUID    string            `json:"codespace_uuid"`
	OperationVersion int64             `json:"operation_version"`
	Workspace        string            `json:"workspace"`
	Selection        Selection         `json:"selection"`
	RuntimeUser      RuntimeUser       `json:"runtime_user"`
	GitUserName      string            `json:"git_user_name,omitempty"`
	GitUserEmail     string            `json:"git_user_email,omitempty"`
	LocalEnvironment map[string]string `json:"local_environment,omitempty"`
	Secrets          map[string]string `json:"secrets,omitempty"`
	Environment      *Environment      `json:"environment,omitempty"`
}

type RuntimeUser struct {
	Name string `json:"name"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`
	Home string `json:"home"`
}

// Environment is the complete local target required for resume and Gateway access.
type Environment struct {
	Version             int               `json:"version"`
	ID                  string            `json:"id"`
	CodespaceUUID       string            `json:"codespace_uuid"`
	ConfigurationPath   string            `json:"configuration_path"`
	ConfigurationSHA256 string            `json:"configuration_sha256"`
	Workspace           string            `json:"workspace"`
	WorkspaceFolder     string            `json:"workspace_folder"`
	ComposeProject      string            `json:"compose_project,omitempty"`
	PrimaryService      string            `json:"primary_service,omitempty"`
	PrimaryContainerID  string            `json:"primary_container_id"`
	RelatedContainerIDs []string          `json:"related_container_ids"`
	Configuration       Configuration     `json:"configuration"`
	RemoteUser          string            `json:"remote_user"`
	RemoteWorkdir       string            `json:"remote_workdir"`
	RemoteEnvironment   map[string]string `json:"remote_environment,omitempty"`
	FeatureDigests      map[string]string `json:"feature_digests,omitempty"`
	Lifecycle           LifecycleState    `json:"lifecycle"`
	WebIDEPort          uint16            `json:"web_ide_port"`
}

func (e *Environment) Validate() error {
	if e == nil || e.Version != RuntimeFormatVersion {
		return formatError("runtime environment version must be %d", RuntimeFormatVersion)
	}
	if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.CodespaceUUID) == "" {
		return formatError("runtime environment identity is incomplete")
	}
	if strings.TrimSpace(e.PrimaryContainerID) == "" {
		return formatError("runtime environment primary container is empty")
	}
	if !strings.HasPrefix(strings.TrimSpace(e.ConfigurationPath), "/") || len(e.ConfigurationSHA256) != 64 {
		return formatError("runtime environment configuration identity is incomplete")
	}
	if !strings.HasPrefix(strings.TrimSpace(e.Workspace), "/") || !strings.HasPrefix(strings.TrimSpace(e.WorkspaceFolder), "/") || !strings.HasPrefix(strings.TrimSpace(e.RemoteWorkdir), "/") {
		return formatError("runtime environment paths must be absolute")
	}
	if strings.TrimSpace(e.RemoteUser) == "" {
		return formatError("runtime environment remote user is empty")
	}
	if e.WebIDEPort == 0 {
		return formatError("runtime environment Web IDE port is empty")
	}
	seen := map[string]struct{}{e.PrimaryContainerID: {}}
	for _, id := range e.RelatedContainerIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return formatError("runtime environment related container is empty")
		}
		if _, ok := seen[id]; ok {
			return formatError("runtime environment contains duplicate container %s", id)
		}
		seen[id] = struct{}{}
	}
	if (e.ComposeProject == "") != (e.PrimaryService == "") {
		return formatError("runtime environment Compose identity is incomplete")
	}
	return nil
}

type LifecycleState struct {
	OnCreateComplete      bool `json:"on_create_complete"`
	UpdateContentComplete bool `json:"update_content_complete"`
	PostCreateComplete    bool `json:"post_create_complete"`
}

type RuntimeResult struct {
	Version     int          `json:"version"`
	Environment *Environment `json:"environment,omitempty"`
	Error       string       `json:"error,omitempty"`
	Recoverable bool         `json:"recoverable,omitempty"`
}

func (r RuntimeRequest) Validate() error {
	if r.Version != RuntimeFormatVersion {
		return formatError("runtime request version must be %d", RuntimeFormatVersion)
	}
	if strings.TrimSpace(r.CodespaceUUID) == "" {
		return formatError("codespace uuid is empty")
	}
	switch r.Action {
	case "create":
		if r.OperationVersion <= 0 {
			return formatError("operation version must be positive")
		}
		if strings.TrimSpace(r.Workspace) == "" {
			return formatError("workspace is empty")
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
			return err
		}
		if r.Environment.CodespaceUUID != r.CodespaceUUID {
			return formatError("runtime environment does not belong to codespace")
		}
	default:
		return formatError("runtime action %q is invalid", r.Action)
	}
	return nil
}

type runtimeFormatError string

func (e runtimeFormatError) Error() string { return string(e) }

func IsFormatError(err error) bool {
	var target runtimeFormatError
	return errors.As(err, &target)
}

func formatError(format string, values ...any) error {
	return runtimeFormatError(fmt.Sprintf(format, values...))
}

// InvalidConfigurationError reports a repository or platform configuration that cannot succeed when retried.
type InvalidConfigurationError struct {
	err error
}

func (e *InvalidConfigurationError) Error() string { return e.err.Error() }
func (e *InvalidConfigurationError) Unwrap() error { return e.err }

func InvalidConfiguration(err error) error {
	if err == nil {
		return nil
	}
	return &InvalidConfigurationError{err: err}
}

func IsInvalidConfiguration(err error) bool {
	var target *InvalidConfigurationError
	return errors.As(err, &target)
}
