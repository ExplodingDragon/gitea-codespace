// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

// Register registers the manager with Gitea and writes codespace.yaml.
func Register(output io.Writer, input io.Reader, configPath string) error {
	if output == nil {
		return fmt.Errorf("output is nil")
	}
	if input == nil {
		return fmt.Errorf("input is nil")
	}

	config, err := LoadConfigForRegister(configPath)
	if err != nil {
		config = DefaultConfig()
		config.applyDefaults()
		config.resolveRelativePaths(configPath)
	}

	reader := bufio.NewReader(input)
	giteaURL, err := promptRequired(output, reader, "Gitea URL", config.Gitea.URL)
	if err != nil {
		return err
	}
	registrationToken, err := promptRequired(output, reader, "Registration token", "")
	if err != nil {
		return err
	}
	managerName, err := promptRequired(output, reader, "Manager name", config.Manager.Name)
	if err != nil {
		return err
	}

	config.Gitea.URL = strings.TrimRight(giteaURL, "/")
	config.Manager.Name = managerName
	if strings.TrimSpace(config.Manager.GatewayURL) == "" {
		config.Manager.GatewayURL = config.Server.PublicBaseURL
	}

	client := codespacev1connect.NewManagerServiceClient(&http.Client{Timeout: config.Manager.HTTPTimeout.ToStdlib()}, config.Gitea.URL)
	ctx, cancel := context.WithTimeout(context.Background(), config.Manager.HTTPTimeout.ToStdlib())
	defer cancel()
	response, err := client.RegisterManager(ctx, connect.NewRequest(&codespacev1.RegisterManagerRequest{
		ProtocolVersion:   1,
		RegistrationToken: registrationToken,
	}))
	if err != nil {
		return fmt.Errorf("register manager rpc: %w", err)
	}

	if err := SaveManagerCredentials(config.Manager.StateDir, ManagerCredentials{
		ManagerID:     response.Msg.GetManagerId(),
		ManagerSecret: response.Msg.GetManagerSecret(),
	}); err != nil {
		return err
	}
	if err := SaveManagerRootState(config.Manager.StateDir, ManagerRootState{
		ManagerID:           response.Msg.GetManagerId(),
		InventoryGeneration: 0,
	}); err != nil {
		return err
	}

	savePath := configPath
	if strings.TrimSpace(savePath) == "" {
		savePath = defaultRegisterConfigPath
	} else {
		savePath = filepath.Join(filepath.Dir(savePath), defaultRegisterConfigPath)
	}
	if err := SaveRegisterConfig(savePath, config); err != nil {
		return err
	}
	fmt.Fprintf(output, "registered manager %d and wrote %s\n", response.Msg.GetManagerId(), savePath)
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
