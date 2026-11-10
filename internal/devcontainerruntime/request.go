// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"gitea.dev/codespace/internal/runtimeplatform"
)

// RunTemplateLifecycleFunc durably executes one environment-template lifecycle action.
type RunTemplateLifecycleFunc func(context.Context, string, func(context.Context) error) error

// NativeContainerConfiguration carries the devices already allocated to the
// outer Runtime Pod into its primary Dev Container.
type NativeContainerConfiguration struct {
	CDIDeviceIDs []string
	Devices      []runtimeplatform.DeviceMapping
	Mounts       []runtimeplatform.MountMapping
	Capabilities []string
	Environment  map[string]string
	ProvidesGPU  bool
}

// Request supplies one Agent lifecycle action to the Dev Container executor.
type Request struct {
	Action               string
	CodespaceUUID        string
	OperationVersion     int64
	Workspace            string
	Source               devcontainer.Source
	HostUser             devcontainer.HostUser
	GitUserName          string
	GitUserEmail         string
	GiteaURL             string
	LocalEnvironment     map[string]string
	Secrets              map[string]string
	DevContainer         Configuration
	Native               NativeContainerConfiguration
	Cache                devcontainer.CacheOptions
	Environment          *devcontainer.State
	RunLifecycle         containerdocker.RunLifecycleFunc
	RunTemplateLifecycle RunTemplateLifecycleFunc
}

// Validate checks action-specific identity and state requirements.
func (r Request) Validate() error {
	if strings.TrimSpace(r.CodespaceUUID) == "" {
		return fmt.Errorf("codespace uuid is empty")
	}
	if r.Action == "create" || r.Action == "resume" {
		giteaURL, err := url.Parse(strings.TrimSpace(r.GiteaURL))
		if err != nil || (giteaURL.Scheme != "http" && giteaURL.Scheme != "https") || giteaURL.Host == "" || giteaURL.User != nil || giteaURL.RawQuery != "" || giteaURL.Fragment != "" {
			return fmt.Errorf("invalid Gitea URL")
		}
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
	case "resume", "stop", "delete", "inspect":
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
		if r.Action == "resume" || r.Action == "stop" {
			if err := r.DevContainer.Validate(); err != nil {
				return fmt.Errorf("invalid Dev Container injection configuration: %w", err)
			}
		}
	default:
		return fmt.Errorf("runtime action %q is invalid", r.Action)
	}
	return nil
}
