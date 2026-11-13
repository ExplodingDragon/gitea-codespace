// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package e2e

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKernelBundleVerification(t *testing.T) {
	fragment, err := os.ReadFile("kata-usbip.config")
	require.NoError(t, err)
	var config strings.Builder
	for line := range strings.SplitSeq(string(fragment), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if symbol, disabled := strings.CutSuffix(line, "=n"); disabled {
			line = "# " + symbol + " is not set"
		}
		fmt.Fprintln(&config, line)
	}
	validConfig := config.String()
	for _, test := range []struct {
		name   string
		config string
		want   string
	}{
		{name: "valid bundle", config: validConfig},
		{name: "missing driver", config: strings.ReplaceAll(validConfig, "CONFIG_USB_STORAGE=y\n", ""), want: "CONFIG_USB_STORAGE=y"},
		{name: "enabled USB networking", config: strings.ReplaceAll(validConfig, "# CONFIG_USB_NET_DRIVERS is not set", "CONFIG_USB_NET_DRIVERS=y"), want: "CONFIG_USB_NET_DRIVERS=n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			var sums strings.Builder
			for name, content := range map[string]string{
				"vmlinuz":  "test kernel",
				"config":   test.config,
				"release":  "test release",
				"metadata": "test metadata",
			} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
				fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256([]byte(content)), name)
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sha256sums"), []byte(sums.String()), 0o600))
			output, err := exec.Command("bash", "build-kata-kernel.sh", "--verify", dir).CombinedOutput()
			if test.want == "" {
				require.NoError(t, err, "%s", output)
			} else {
				require.Error(t, err)
				require.Contains(t, string(output), "required kernel config is missing: "+test.want)
			}
		})
	}
}
