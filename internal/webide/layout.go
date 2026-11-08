// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package webide owns the platform Web IDE program and persistent data layout.
package webide

const (
	BuiltinProgramPath   = "/usr/local/libexec/gitea-codespace/web-ide"
	PersistentRootPath   = "/var/lib/codespace/web-ide"
	CustomProgramPath    = PersistentRootPath + "/program"
	UserDataSourcePath   = PersistentRootPath + "/user-data"
	ExtensionsSourcePath = PersistentRootPath + "/extensions"

	ContainerProgramPath    = "/usr/local/libexec/gitea-codespace/web-ide"
	ContainerUserDataPath   = "/var/lib/gitea-codespace/web-ide/user-data"
	ContainerExtensionsPath = "/var/lib/gitea-codespace/web-ide/extensions"
	PlatformVSIXDirectory   = "/usr/local/share/gitea-codespace/web-ide"
	PlatformVSIXPath        = PlatformVSIXDirectory + "/gitea-codespace.vsix"
	ContainerVSIXDirectory  = PlatformVSIXDirectory
	ContainerVSIXPath       = ContainerVSIXDirectory + "/gitea-codespace.vsix"
)
