// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package webide owns the provider bundle contract and persistent data layout.
package webide

const (
	ProgramRootPath      = "/opt/gitea-codespace/web-ide"
	PersistentRootPath   = "/var/lib/codespace/web-ide"
	ContainerProgramPath = "/usr/local/libexec/gitea-codespace/web-ide"
	ContainerDataPath    = "/var/lib/gitea-codespace/web-ide"
	ManifestName         = "manifest.json"
	PrepareProgram       = "bin/prepare"
	ServeProgram         = "bin/serve"
)
