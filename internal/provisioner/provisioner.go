// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"context"
	"path/filepath"
	"strings"
)

// RuntimeState stores the locally observed runtime state.
type RuntimeState string

const (
	// RuntimeStateCreating means the runtime identity exists but startup is not stable yet.
	RuntimeStateCreating RuntimeState = "creating"
	// RuntimeStateRunning means the runtime is running.
	RuntimeStateRunning RuntimeState = "running"
	// RuntimeStateStopped means the runtime is stopped but still recoverable.
	RuntimeStateStopped RuntimeState = "stopped"
	// RuntimeStateFailed means the runtime identity exists but cannot be recovered.
	RuntimeStateFailed RuntimeState = "failed"
)

// Instance stores one provisioned codespace instance.
type Instance struct {
	CodespaceUUID          string
	Name                   string
	RuntimeState           RuntimeState
	Workdir                string
	RepoFullName           string
	RepoTag                string
	InternalSSHHost        string
	InternalSSHPort        int
	InternalSSHUser        string
	InternalSSHAuthMode    string
	InternalSSHFingerprint string
}

// InstanceSpec stores the runtime instance shape requested by Gitea.
type InstanceSpec struct {
	CodespaceUUID string
	Name          string
	RepoFullName  string
	RepoTag       string
}

// BootstrapRequest stores the codespace bootstrap inputs.
type BootstrapRequest struct {
	CodespaceUUID    string
	GiteaToken       string
	ServerURL        string
	RepoCloneHTTPURL string
	RepoCloneSSHURL  string
	RepoFullName     string
	StartRef         string
	CommitSHA        string
	Workdir          string
	GitProtocol      string
}

// BootstrapConfig stores runtime bootstrap execution settings.
type BootstrapConfig struct {
	Shell   string
	HomeDir string
	User    uint32
	Group   uint32
}

// Provisioner creates and manages codespace instances.
type Provisioner interface {
	CreateOrStart(ctx context.Context, spec InstanceSpec) (*Instance, error)
	StartExisting(ctx context.Context, spec InstanceSpec) (*Instance, error)
	ListInstances(ctx context.Context) ([]*Instance, error)
	Bootstrap(ctx context.Context, instanceName string, request BootstrapRequest) error
	Stop(ctx context.Context, instanceName string) error
	Delete(ctx context.Context, instanceName string) error
}

func repoDirName(repoFullName string) string {
	repoFullName = strings.Trim(repoFullName, "/")
	if repoFullName == "" {
		return "repo"
	}
	return filepath.Base(repoFullName)
}
