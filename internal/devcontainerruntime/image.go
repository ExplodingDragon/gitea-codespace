// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	containerdocker "gitea.dev/codespace/devcontainer/docker"
)

const (
	platformGiteaCLIPath        = "/usr/local/libexec/gitea-codespace/gt"
	templateLifecycleScriptPath = "/usr/local/lib/gitea-codespace/lifecycle.sh"
)

func imageCustomization(configuration Configuration, giteaCLIPath string, output io.Writer) containerdocker.CustomizeImageFunc {
	hasLifecycleScript := strings.TrimSpace(configuration.LifecycleScript) != ""
	if !configuration.GiteaCLI && !hasLifecycleScript {
		return nil
	}
	if output == nil {
		output = io.Discard
	}
	return func(image containerdocker.ImageCustomization) error {
		if configuration.GiteaCLI {
			if image.Platform.OS != runtime.GOOS || image.Platform.Architecture != runtime.GOARCH {
				return fmt.Errorf("platform Gitea CLI is built for %s/%s, target image is %s/%s", runtime.GOOS, runtime.GOARCH, image.Platform.OS, image.Platform.Architecture)
			}
			info, err := os.Lstat(giteaCLIPath)
			if err != nil {
				return fmt.Errorf("platform image does not contain the Gitea CLI asset: %w", err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("platform Gitea CLI asset %s is not a regular file", giteaCLIPath)
			}
			source, err := os.Open(giteaCLIPath)
			if err != nil {
				return fmt.Errorf("open platform Gitea CLI asset: %w", err)
			}
			destination, err := os.OpenFile(filepath.Join(image.ContextDir, "gt"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				_ = source.Close()
				return fmt.Errorf("create Gitea CLI build asset: %w", err)
			}
			_, copyErr := io.Copy(destination, source)
			if err := errors.Join(copyErr, destination.Close(), source.Close()); err != nil {
				_ = os.Remove(filepath.Join(image.ContextDir, "gt"))
				return fmt.Errorf("stage Gitea CLI build asset: %w", err)
			}
			_, _ = fmt.Fprintln(output, "Add the platform Gitea CLI")
		}

		var dockerfile strings.Builder
		_, _ = fmt.Fprintf(&dockerfile, "FROM %s\nUSER root\n", image.BaseImage)
		if configuration.GiteaCLI {
			dockerfile.WriteString("COPY gt /usr/local/bin/gt\n")
			dockerfile.WriteString("RUN chmod 0755 /usr/local/bin/gt \\\n")
			dockerfile.WriteString("    && /usr/local/bin/gt version \\\n")
			dockerfile.WriteString("    && mkdir -p /usr/local/share/bash-completion/completions /usr/local/share/zsh/site-functions /usr/local/share/fish/vendor_completions.d \\\n")
			dockerfile.WriteString("    && /usr/local/bin/gt completion bash > /usr/local/share/bash-completion/completions/gt \\\n")
			dockerfile.WriteString("    && /usr/local/bin/gt completion zsh > /usr/local/share/zsh/site-functions/_gt \\\n")
			dockerfile.WriteString("    && /usr/local/bin/gt completion fish > /usr/local/share/fish/vendor_completions.d/gt.fish\n")
		}
		if hasLifecycleScript {
			_, _ = fmt.Fprintf(&dockerfile, "COPY lifecycle.sh %s\nRUN chmod 0500 %s && /bin/sh %s build\n", templateLifecycleScriptPath, templateLifecycleScriptPath, templateLifecycleScriptPath)
		}
		_, _ = fmt.Fprintf(&dockerfile, "USER %s\n", image.ContainerUser)
		if err := os.WriteFile(filepath.Join(image.ContextDir, "Dockerfile"), []byte(dockerfile.String()), 0o600); err != nil {
			return err
		}
		if !hasLifecycleScript {
			return nil
		}
		return os.WriteFile(filepath.Join(image.ContextDir, "lifecycle.sh"), []byte(configuration.LifecycleScript), 0o600)
	}
}
