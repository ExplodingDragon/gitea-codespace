// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/controlplane"
)

// Register registers the manager with Gitea and writes the state directory.
func Register(output io.Writer, input io.Reader, configPath string) error {
	if output == nil {
		return fmt.Errorf("output is nil")
	}
	if input == nil {
		return fmt.Errorf("input is nil")
	}

	config, err := LoadConfigForRegister(configPath)
	if err != nil {
		return fmt.Errorf("load register config: %w", err)
	}
	if strings.TrimSpace(config.Manager.GatewayURL) == "" {
		config.Manager.GatewayURL = config.Server.PublicBaseURL
	}
	if err := config.Manager.Validate(); err != nil {
		return fmt.Errorf("validate manager config: %w", err)
	}

	stateLock, err := acquireStateDirLock(config.Manager.StateDir)
	if err != nil {
		return fmt.Errorf("acquire manager state dir lock: %w", err)
	}
	defer func() {
		_ = stateLock.Close()
	}()
	if err := preflightManagerStateDir(config.Manager.StateDir); err != nil {
		return err
	}

	reader := bufio.NewReader(input)
	giteaURL, err := promptRequired(output, reader, "Gitea URL", "")
	if err != nil {
		return err
	}
	giteaURL, err = normalizeGiteaURL(giteaURL)
	if err != nil {
		return err
	}
	registrationToken, err := promptRequired(output, reader, "Registration token", "")
	if err != nil {
		return err
	}

	client := codespacev1connect.NewManagerServiceClient(&http.Client{Timeout: config.Manager.HTTPTimeout.ToStdlib()}, managerServiceBaseURL(giteaURL))
	ctx, cancel := context.WithTimeout(context.Background(), config.Manager.HTTPTimeout.ToStdlib())
	defer cancel()
	response, err := client.RegisterManager(ctx, connect.NewRequest(&codespacev1.RegisterManagerRequest{
		ProtocolVersion:   controlplane.ProtocolVersion,
		RegistrationToken: registrationToken,
	}))
	if err != nil {
		return fmt.Errorf("register manager rpc: %w", err)
	}

	managerState := ManagerState{
		GiteaURL:       giteaURL,
		ManagerID:      response.Msg.GetManagerId(),
		ManagerSecret:  response.Msg.GetManagerSecret(),
		RegisteredUnix: time.Now().Unix(),
	}
	if err := SaveManagerState(config.Manager.StateDir, managerState); err != nil {
		return fmt.Errorf("save registered manager %d state: %w; remove the unused Manager in Gitea before registering again", managerState.ManagerID, err)
	}

	fmt.Fprintf(output, "registered manager %d and wrote state %s\n", response.Msg.GetManagerId(), config.Manager.StateDir)
	return nil
}

func preflightManagerStateDir(stateDir string) error {
	path, err := managerStatePath(stateDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		state, loadErr := LoadManagerState(stateDir)
		if loadErr != nil {
			return fmt.Errorf("manager state already exists but is invalid: %w", loadErr)
		}
		return fmt.Errorf("manager state is already registered with %s as manager %d", state.GiteaURL, state.ManagerID)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat manager state %s: %w", path, err)
	}

	tempFile, err := os.CreateTemp(stateDir, ".manager-state.preflight.*")
	if err != nil {
		return fmt.Errorf("create manager state preflight file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.WriteString("manager state preflight\n"); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write manager state preflight file: %w", err)
	}
	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("chmod manager state preflight file: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync manager state preflight file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close manager state preflight file: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("remove manager state preflight file: %w", err)
	}
	if err := syncStateDir(stateDir); err != nil {
		return fmt.Errorf("sync manager state directory: %w", err)
	}
	return nil
}

func promptRequired(output io.Writer, reader *bufio.Reader, label, defaultValue string) (string, error) {
	for {
		if strings.TrimSpace(defaultValue) == "" {
			fmt.Fprintf(output, "%s: ", label)
		} else {
			fmt.Fprintf(output, "%s [%s]: ", label, defaultValue)
		}
		value, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("read %s: %w", label, err)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			value = strings.TrimSpace(defaultValue)
		}
		if value != "" {
			return value, nil
		}
		if err == io.EOF {
			return "", fmt.Errorf("%s is required", label)
		}
	}
}
