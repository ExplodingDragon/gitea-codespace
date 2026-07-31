// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"fmt"
	"strings"
	"unicode"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/provisioner"
)

func startupInputFromCreatePayload(operation *codespacev1.OperationPayload, payload *codespacev1.CreateOperationPayload) (StartupInput, error) {
	if operation == nil {
		return StartupInput{}, fmt.Errorf("operation is required")
	}
	if payload == nil {
		return StartupInput{}, fmt.Errorf("create payload is required")
	}
	username := strings.TrimSpace(payload.GetUsername())
	gitUserEmail := strings.TrimSpace(payload.GetGitUserEmail())
	if username == "" {
		return StartupInput{}, fmt.Errorf("create username is required")
	}
	if gitUserEmail == "" {
		return StartupInput{}, fmt.Errorf("create git user email is required")
	}
	repoFullName := strings.Trim(strings.TrimSpace(payload.GetRepoFullName()), "/")
	if repoFullName == "" || !strings.Contains(repoFullName, "/") {
		return StartupInput{}, fmt.Errorf("create repository full name is invalid")
	}
	environmentTag := strings.TrimSpace(payload.GetEnvironmentTag())
	if environmentTag == "" {
		return StartupInput{}, fmt.Errorf("create environment tag is required")
	}
	devContainer := payload.GetDevContainer()
	if devContainer == nil {
		return StartupInput{}, fmt.Errorf("create Dev Container configuration is required")
	}
	startupInput := StartupInput{
		CodespaceUUID:   operation.GetCodespaceUuid(),
		RepoFullName:    repoFullName,
		Username:        username,
		GitUserEmail:    gitUserEmail,
		RuntimeUserName: deriveRuntimeUserName(username),
		EnvironmentTag:  environmentTag,
	}
	startupInput.DevContainer.Path = strings.TrimSpace(devContainer.GetRepositoryPath())
	startupInput.DevContainer.ContentSHA256 = strings.TrimSpace(devContainer.GetRepositoryContentSha256())
	startupInput.DevContainer.DefaultImage = strings.TrimSpace(devContainer.GetDefaultImage())
	switch {
	case startupInput.DevContainer.DefaultImage != "":
		startupInput.DevContainer.Source = provisioner.DevContainerSourcePlatformDefault
	case startupInput.DevContainer.Path != "" || startupInput.DevContainer.ContentSHA256 != "":
		startupInput.DevContainer.Source = provisioner.DevContainerSourceRepository
		startupInput.DevContainer.CommitSHA = strings.TrimSpace(payload.GetCommitSha())
	default:
		return StartupInput{}, fmt.Errorf("create Dev Container configuration source is invalid")
	}
	if err := startupInput.DevContainer.Validate(); err != nil {
		return StartupInput{}, fmt.Errorf("create Dev Container configuration is invalid: %w", err)
	}
	return startupInput, nil
}

func deriveRuntimeUserName(username string) string {
	username = strings.ToLower(strings.TrimSpace(username))
	var builder strings.Builder
	lastSeparator := false
	for _, r := range username {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastSeparator = false
		case r == '_' || r == '-':
			if !lastSeparator {
				builder.WriteByte('-')
				lastSeparator = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			continue
		default:
			if !lastSeparator {
				builder.WriteByte('-')
				lastSeparator = true
			}
		}
	}
	value := strings.Trim(builder.String(), "-_")
	if value == "" {
		value = "codespace"
	}
	if value[0] >= '0' && value[0] <= '9' {
		value = "u-" + value
	}
	if isReservedRuntimeUserName(value) {
		value = "u-" + value
	}
	if len(value) > 32 {
		value = strings.Trim(value[:32], "-_")
	}
	if value == "" {
		return "codespace"
	}
	return value
}

func isReservedRuntimeUserName(username string) bool {
	switch username {
	case "root", "daemon", "bin", "sys", "sync", "games", "man", "lp", "mail", "news", "uucp", "proxy", "www-data", "backup", "list", "irc", "gnats", "nobody":
		return true
	default:
		return false
	}
}
