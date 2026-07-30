// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"errors"
	"fmt"
	"strings"
)

// StateFormatVersion is the only state representation accepted by this package.
const StateFormatVersion = 1

// HostUser is the local identity whose UID and GID are mapped to remoteUser.
type HostUser struct {
	Name string `json:"name"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`
	Home string `json:"home"`
}

// State contains the Docker resources and resolved settings required to reopen an environment.
type State struct {
	Version             int               `json:"version"`
	ID                  string            `json:"id"`
	OwnerID             string            `json:"owner_id"`
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
}

// Validate checks the state identity and the resources required for future operations.
func (s *State) Validate() error {
	if s == nil || s.Version != StateFormatVersion {
		return fmt.Errorf("Dev Container state version must be %d", StateFormatVersion)
	}
	if strings.TrimSpace(s.ID) == "" || strings.TrimSpace(s.OwnerID) == "" {
		return fmt.Errorf("Dev Container state identity is incomplete")
	}
	if strings.TrimSpace(s.PrimaryContainerID) == "" {
		return fmt.Errorf("Dev Container primary container is empty")
	}
	if !strings.HasPrefix(strings.TrimSpace(s.ConfigurationPath), "/") || len(s.ConfigurationSHA256) != 64 {
		return fmt.Errorf("Dev Container configuration identity is incomplete")
	}
	if !strings.HasPrefix(strings.TrimSpace(s.Workspace), "/") || !strings.HasPrefix(strings.TrimSpace(s.WorkspaceFolder), "/") || !strings.HasPrefix(strings.TrimSpace(s.RemoteWorkdir), "/") {
		return fmt.Errorf("Dev Container state paths must be absolute")
	}
	if strings.TrimSpace(s.RemoteUser) == "" {
		return fmt.Errorf("Dev Container remote user is empty")
	}
	seen := map[string]struct{}{s.PrimaryContainerID: {}}
	for _, id := range s.RelatedContainerIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return fmt.Errorf("Dev Container related container is empty")
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("Dev Container state contains duplicate container %s", id)
		}
		seen[id] = struct{}{}
	}
	if (s.ComposeProject == "") != (s.PrimaryService == "") {
		return fmt.Errorf("Dev Container Compose identity is incomplete")
	}
	return nil
}

// LifecycleState records one-time lifecycle commands that completed successfully.
type LifecycleState struct {
	OnCreateComplete      bool `json:"on_create_complete"`
	UpdateContentComplete bool `json:"update_content_complete"`
	PostCreateComplete    bool `json:"post_create_complete"`
}

// InvalidConfigurationError identifies repository configuration that cannot succeed when retried.
type InvalidConfigurationError struct {
	err error
}

func (e *InvalidConfigurationError) Error() string { return e.err.Error() }
func (e *InvalidConfigurationError) Unwrap() error { return e.err }

// InvalidConfiguration marks an error as deterministic repository configuration failure.
func InvalidConfiguration(err error) error {
	if err == nil {
		return nil
	}
	return &InvalidConfigurationError{err: err}
}

// IsInvalidConfiguration reports whether retrying without a configuration change cannot succeed.
func IsInvalidConfiguration(err error) bool {
	var target *InvalidConfigurationError
	return errors.As(err, &target)
}
