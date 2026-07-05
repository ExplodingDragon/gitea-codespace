// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"path/filepath"
	"strings"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

// Instance stores one provisioned codespace instance.
type Instance struct {
	Name         string
	State        codespacev1.CodespaceStatus
	Workdir      string
	Image        string
	Type         string
	RepoFullName string
}

// InstanceSpec stores the runtime instance shape requested by Gitea.
type InstanceSpec struct {
	Name         string
	Type         string
	Image        string
	RepoFullName string
}

// BootstrapRequest stores the codespace bootstrap inputs.
type BootstrapRequest struct {
	CodespaceID   string
	RuntimeToken  string
	RuntimeAPIURL string
	RepoURL       string
	RepoFullName  string
	StartRef      string
	StartSHA      string
	Workdir       string
	InitScript    string
	GitUsername   string
	GitToken      string
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
	CreateOrStart(spec InstanceSpec) (*Instance, error)
	StartExisting(spec InstanceSpec) (*Instance, error)
	Bootstrap(instanceName string, request BootstrapRequest) error
	Stop(instanceName string) error
	Delete(instanceName string) error
}

func repoDirName(repoFullName string) string {
	repoFullName = strings.Trim(repoFullName, "/")
	if repoFullName == "" {
		return "repo"
	}
	return filepath.Base(repoFullName)
}
