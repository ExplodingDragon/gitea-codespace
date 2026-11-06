// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// StateFormatVersion is the only state representation accepted by this package.
const StateFormatVersion = 2

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
	ComposeContainers   map[string]string `json:"compose_containers,omitempty"`
	PrimaryContainerID  string            `json:"primary_container_id"`
	RelatedContainerIDs []string          `json:"related_container_ids"`
	Configuration       Configuration     `json:"configuration"`
	RemoteUser          string            `json:"remote_user"`
	RemoteWorkdir       string            `json:"remote_workdir"`
	RemoteEnvironment   map[string]string `json:"remote_environment,omitempty"`
	FeatureDigests      map[string]string `json:"feature_digests,omitempty"`
	LifecycleCommands   LifecycleCommands `json:"lifecycle_commands"`
}

// IsValidWorkspaceFolder reports whether value is a canonical absolute path
// inside a Dev Container.
func IsValidWorkspaceFolder(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= 4096 && utf8.ValidString(value) &&
		path.IsAbs(value) && path.Clean(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

// Validate checks the state identity and the resources required for future operations.
func (s *State) Validate() error {
	if s == nil || s.Version != StateFormatVersion {
		return fmt.Errorf("dev container state version must be %d", StateFormatVersion)
	}
	if strings.TrimSpace(s.ID) == "" || strings.TrimSpace(s.OwnerID) == "" {
		return fmt.Errorf("dev container state identity is incomplete")
	}
	if strings.TrimSpace(s.PrimaryContainerID) == "" {
		return fmt.Errorf("dev container primary container is empty")
	}
	if !strings.HasPrefix(strings.TrimSpace(s.ConfigurationPath), "/") || len(s.ConfigurationSHA256) != 64 {
		return fmt.Errorf("dev container configuration identity is incomplete")
	}
	if !strings.HasPrefix(strings.TrimSpace(s.Workspace), "/") || !strings.HasPrefix(strings.TrimSpace(s.RemoteWorkdir), "/") ||
		!IsValidWorkspaceFolder(s.WorkspaceFolder) {
		return fmt.Errorf("dev container state paths must be absolute")
	}
	if strings.TrimSpace(s.RemoteUser) == "" {
		return fmt.Errorf("dev container remote user is empty")
	}
	if len(s.Configuration.OnCreateCommand.Value) != 0 ||
		len(s.Configuration.UpdateContentCommand.Value) != 0 ||
		len(s.Configuration.PostCreateCommand.Value) != 0 ||
		len(s.Configuration.PostStartCommand.Value) != 0 ||
		len(s.Configuration.PostAttachCommand.Value) != 0 {
		return fmt.Errorf("dev container state contains flattened lifecycle commands")
	}
	seen := map[string]struct{}{s.PrimaryContainerID: {}}
	for _, id := range s.RelatedContainerIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return fmt.Errorf("dev container related container is empty")
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("dev container state contains duplicate container %s", id)
		}
		seen[id] = struct{}{}
	}
	if (s.ComposeProject == "") != (s.PrimaryService == "") {
		return fmt.Errorf("dev container Compose identity is incomplete")
	}
	if s.ComposeProject == "" {
		if len(s.ComposeContainers) != 0 {
			return fmt.Errorf("non-Compose Dev Container contains Compose services")
		}
	} else {
		if s.ComposeContainers[s.PrimaryService] != s.PrimaryContainerID {
			return fmt.Errorf("dev container primary Compose service is incomplete")
		}
		mapped := make(map[string]string, len(s.ComposeContainers))
		for service, id := range s.ComposeContainers {
			if strings.TrimSpace(service) == "" {
				return fmt.Errorf("dev container Compose service is empty")
			}
			if _, ok := seen[id]; !ok {
				return fmt.Errorf("dev container Compose service %s references an unknown container", service)
			}
			if existing := mapped[id]; existing != "" {
				return fmt.Errorf("dev container Compose services %s and %s reference the same container", existing, service)
			}
			mapped[id] = service
		}
		if len(mapped) != len(seen) {
			return fmt.Errorf("dev container Compose service mapping is incomplete")
		}
	}
	return nil
}

// LifecycleCommands preserves command sources in their required execution order.
// Commands within one entry retain the Dev Container object form and may run in parallel.
type LifecycleCommands struct {
	OnCreate      []Command `json:"on_create,omitempty"`
	UpdateContent []Command `json:"update_content,omitempty"`
	PostCreate    []Command `json:"post_create,omitempty"`
	PostStart     []Command `json:"post_start,omitempty"`
	PostAttach    []Command `json:"post_attach,omitempty"`
}

// Append adds the lifecycle commands contributed by one metadata source.
func (c *LifecycleCommands) Append(configuration Configuration) {
	appendCommand := func(commands *[]Command, command Command) {
		if len(command.Value) != 0 {
			*commands = append(*commands, Command{Value: slices.Clone(command.Value)})
		}
	}
	appendCommand(&c.OnCreate, configuration.OnCreateCommand)
	appendCommand(&c.UpdateContent, configuration.UpdateContentCommand)
	appendCommand(&c.PostCreate, configuration.PostCreateCommand)
	appendCommand(&c.PostStart, configuration.PostStartCommand)
	appendCommand(&c.PostAttach, configuration.PostAttachCommand)
}
