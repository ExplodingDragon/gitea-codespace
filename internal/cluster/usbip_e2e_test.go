// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "embed"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/types"
)

//go:embed usbip-e2e.sh
var usbIPE2EScript []byte

//go:embed usbip-devcontainer.Dockerfile
var usbIPE2EDevContainerDockerfile []byte

func verifyGatewayUSBIP(t *testing.T, ctx context.Context, node string, port int32, runtimeUUID string, privateKey ed25519.PrivateKey, runtimePod types.NamespacedName) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	work := t.TempDir()
	keyBlock, err := ssh.MarshalPrivateKey(privateKey, "codespace-usbip-e2e")
	if err != nil {
		return fmt.Errorf("marshal USB/IP SSH key: %w", err)
	}
	keyPath := filepath.Join(work, "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(keyBlock), 0o600); err != nil {
		return err
	}
	socketPath := filepath.Join(work, "control.sock")
	host := "cs-" + runtimeUUID + "@" + node
	common := []string{"-o", "ControlPath=" + socketPath, "-o", "UserKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=no", "-o", "LogLevel=ERROR", "-i", keyPath, "-p", strconv.Itoa(int(port)), host}
	type forwardProcess struct {
		command *exec.Cmd
		output  *bytes.Buffer
		wait    <-chan error
	}
	startForward := func() (*forwardProcess, error) {
		args := append([]string{"-N", "-T", "-o", "ControlMaster=yes", "-o", "ExitOnForwardFailure=yes", "-R", "usbip.workstation:3240:127.0.0.1:3240", "-R", "usbip.lab:3240:127.0.0.1:3241", "-R", "127.0.0.1:3240:127.0.0.1:3240"}, common...)
		command := exec.CommandContext(ctx, "ssh", args...)
		output := new(bytes.Buffer)
		command.Stdout = output
		command.Stderr = output
		command.Stdin = strings.NewReader("")
		if err := command.Start(); err != nil {
			return nil, err
		}
		wait := make(chan error, 1)
		go func() { wait <- command.Wait() }()
		for attempt := 0; attempt < 120; attempt++ {
			check := exec.CommandContext(ctx, "ssh", append([]string{"-O", "check"}, common...)...)
			if check.Run() == nil {
				return &forwardProcess{command: command, output: output, wait: wait}, nil
			}
			select {
			case err := <-wait:
				return nil, fmt.Errorf("USB/IP SSH reverse forwarding exited before becoming ready: %s: %w", strings.TrimSpace(output.String()), err)
			case <-ctx.Done():
				_ = command.Process.Kill()
				<-wait
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		_ = command.Process.Kill()
		<-wait
		return nil, fmt.Errorf("USB/IP SSH reverse forwarding did not become ready: %s", strings.TrimSpace(output.String()))
	}
	stopForward := func(forward *forwardProcess) error {
		// OpenSSH gives a forwarding-only session a clean exit status on SIGTERM.
		if err := forward.command.Process.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("terminate USB/IP SSH reverse forwarding: %w", err)
		}
		if err := <-forward.wait; err != nil && ctx.Err() == nil {
			return fmt.Errorf("stop USB/IP SSH reverse forwarding: %s: %w", strings.TrimSpace(forward.output.String()), err)
		}
		return nil
	}
	changeForward := func(command string) error {
		args := []string{"-O", command, "-R", "usbip.workstation:3240:127.0.0.1:3240"}
		output, err := exec.CommandContext(ctx, "ssh", append(args, common...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("USB/IP SSH reverse forwarding %s: %s: %w", command, strings.TrimSpace(string(output)), err)
		}
		return nil
	}
	runScript := func(argument string) error {
		args := append([]string{}, common...)
		commandLine := "exec sh -s --"
		if argument != "" {
			commandLine += " " + argument
		}
		args = append(args, commandLine)
		command := exec.CommandContext(ctx, "ssh", args...)
		command.Stdin = bytes.NewReader(usbIPE2EScript)
		output, err := command.CombinedOutput()
		if err != nil {
			// Capture the Guest state before closing SSH forwarding or deleting the Pod.
			diagnosticCtx, diagnosticCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer diagnosticCancel()
			diagnostic, diagnosticErr := exec.CommandContext(diagnosticCtx, "kubectl", "exec", "-n", runtimePod.Namespace, runtimePod.Name, "-c", "runtime", "--", "sh", "-c", `
set -x
usbip --debug port
for status in /sys/devices/platform/vhci_hcd.*/status*; do
  printf '\n%s\n' "$status"
  cat "$status"
done
dmesg | tail -n 100
`).CombinedOutput()
			t.Logf("USB/IP Runtime diagnostics (%s, exec error: %v):\n%s", runtimePod, diagnosticErr, diagnostic)
			return fmt.Errorf("run USB/IP product script: %s: %w", strings.TrimSpace(string(output)), err)
		}
		return nil
	}
	runRemoteCommand := func(commandLine string) error {
		command := exec.CommandContext(ctx, "ssh", append(append([]string{}, common...), commandLine)...)
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("run USB/IP SSH command: %s: %w", strings.TrimSpace(string(output)), err)
		}
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		forward, err := startForward()
		if err != nil {
			return err
		}
		if err := runScript(""); err != nil {
			_ = stopForward(forward)
			return err
		}
		if attempt == 0 {
			if err := changeForward("cancel"); err != nil {
				_ = stopForward(forward)
				return err
			}
			if err := runScript("check-lab"); err != nil {
				_ = stopForward(forward)
				return err
			}
			if err := changeForward("forward"); err != nil {
				_ = stopForward(forward)
				return err
			}
			if err := runScript(""); err != nil {
				_ = stopForward(forward)
				return err
			}
		}
		if err := runRemoteCommand("gitea-codespace-usbip attach 1-1 --source workstation"); err != nil {
			_ = stopForward(forward)
			return err
		}
		if err := stopForward(forward); err != nil {
			return err
		}
		if err := runScript("check-disconnected"); err != nil {
			return err
		}
	}
	return nil
}
