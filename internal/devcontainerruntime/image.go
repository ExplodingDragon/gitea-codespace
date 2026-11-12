// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	containerdocker "gitea.dev/codespace/devcontainer/docker"
)

const templateLifecycleScriptPath = "/usr/local/lib/gitea-codespace/lifecycle.sh"

func imageCustomization(lifecycleScript string) containerdocker.CustomizeImageFunc {
	if strings.TrimSpace(lifecycleScript) == "" {
		return nil
	}
	return func(image containerdocker.ImageCustomization) error {
		dockerfile := fmt.Sprintf("FROM %s\nUSER root\nCOPY lifecycle.sh %s\nRUN chmod 0500 %s && /bin/sh %s build\nUSER %s\n",
			image.BaseImage, templateLifecycleScriptPath, templateLifecycleScriptPath, templateLifecycleScriptPath, image.ContainerUser)
		if err := os.WriteFile(filepath.Join(image.ContextDir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(image.ContextDir, "lifecycle.sh"), []byte(lifecycleScript), 0o600)
	}
}
