// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"errors"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"
)

func parseGatewaySSHPty(payload []byte) gatewaySSHPty {
	var request struct {
		Term     string
		Cols     uint32
		Rows     uint32
		Width    uint32
		Height   uint32
		Modelist string
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return gatewaySSHPty{enabled: true, cols: defaultGatewaySSHCols, rows: defaultGatewaySSHRows}
	}
	cols := int(request.Cols)
	rows := int(request.Rows)
	if cols <= 0 {
		cols = defaultGatewaySSHCols
	}
	if rows <= 0 {
		rows = defaultGatewaySSHRows
	}
	return gatewaySSHPty{enabled: true, cols: cols, rows: rows}
}

func parseGatewaySSHWindowChange(payload []byte, fallback gatewaySSHPty) gatewaySSHPty {
	var request struct {
		Cols   uint32
		Rows   uint32
		Width  uint32
		Height uint32
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return fallback
	}
	cols := int(request.Cols)
	rows := int(request.Rows)
	if cols <= 0 {
		cols = fallback.cols
	}
	if rows <= 0 {
		rows = fallback.rows
	}
	if cols <= 0 {
		cols = defaultGatewaySSHCols
	}
	if rows <= 0 {
		rows = defaultGatewaySSHRows
	}
	return gatewaySSHPty{enabled: true, cols: cols, rows: rows}
}

func parseGatewaySSHExecCommand(payload []byte) string {
	var request struct {
		Command string
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return ""
	}
	return request.Command
}

func parseGatewaySSHSubsystem(payload []byte) string {
	var request struct {
		Name string
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return ""
	}
	return request.Name
}

func parseGatewaySSHSignal(payload []byte) (int, bool) {
	var request struct {
		Signal string
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return 0, false
	}
	switch strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(request.Signal)), "SIG") {
	case "ABRT":
		return int(syscall.SIGABRT), true
	case "ALRM":
		return int(syscall.SIGALRM), true
	case "FPE":
		return int(syscall.SIGFPE), true
	case "HUP":
		return int(syscall.SIGHUP), true
	case "ILL":
		return int(syscall.SIGILL), true
	case "INT":
		return int(syscall.SIGINT), true
	case "KILL":
		return int(syscall.SIGKILL), true
	case "PIPE":
		return int(syscall.SIGPIPE), true
	case "QUIT":
		return int(syscall.SIGQUIT), true
	case "SEGV":
		return int(syscall.SIGSEGV), true
	case "TERM":
		return int(syscall.SIGTERM), true
	case "USR1":
		return int(syscall.SIGUSR1), true
	case "USR2":
		return int(syscall.SIGUSR2), true
	default:
		return 0, false
	}
}

func gatewaySSHExitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *WorkspaceCommandExitError
	if errors.As(err, &exitErr) && exitErr.Status >= 0 && exitErr.Status <= 255 {
		return exitErr.Status
	}
	return 255
}

func notifyGatewaySSHActivity(activity chan<- struct{}) {
	select {
	case activity <- struct{}{}:
	default:
	}
}

func gatewaySSHAuthFromPermissions(permissions *ssh.Permissions) (gatewaySSHAuthContext, bool) {
	if permissions == nil {
		return gatewaySSHAuthContext{}, false
	}
	codespaceUUID := permissions.Extensions["codespace_uuid"]
	userID, err := strconv.ParseInt(strings.TrimSpace(permissions.Extensions["user_id"]), 10, 64)
	if err != nil || codespaceUUID == "" || userID <= 0 {
		return gatewaySSHAuthContext{}, false
	}
	return gatewaySSHAuthContext{codespaceUUID: codespaceUUID, userID: userID}, true
}

func codespaceUUIDFromGatewaySSHUser(user string) (string, bool) {
	codespaceUUID, ok := strings.CutPrefix(user, gatewaySSHUserPrefix)
	if !ok {
		return "", false
	}
	if err := ValidateRuntimeUUID(codespaceUUID); err != nil {
		return "", false
	}
	return codespaceUUID, true
}
