// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"io"
	"net"
	"strconv"
)

// A shell request carries the terminal settings for one SSH login shell.
type workspaceShellRequest struct {
	RuntimeUUID string
	Interactive bool
	Cols        int
	Rows        int
}

// A command request carries one SSH exec command and its terminal settings.
type workspaceCommandRequest struct {
	RuntimeUUID string
	Command     string
	Interactive bool
	Cols        int
	Rows        int
}

// An SFTP request identifies the Runtime that receives the stream.
type workspaceSFTPRequest struct {
	RuntimeUUID string
}

// A command session presents a bidirectional Runtime process to SSH.
type workspaceCommandSession interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Resize(cols, rows int) error
	Signal(signal int) error
	Wait() error
	Close() error
}

// The command exit error carries a process status through the SSH adapter.
type workspaceCommandExitError struct {
	Status int
}

func (e *workspaceCommandExitError) Error() string {
	return "workspace command exited with status " + strconv.Itoa(e.Status)
}

// The workspace backend opens each supported stream to a Runtime Agent.
type workspaceBackend interface {
	OpenWorkspaceShell(context.Context, workspaceShellRequest) (workspaceCommandSession, error)
	OpenWorkspaceCommand(context.Context, workspaceCommandRequest) (workspaceCommandSession, error)
	OpenWorkspaceSFTP(context.Context, workspaceSFTPRequest) (io.ReadWriteCloser, error)
	OpenWorkspaceEndpoint(context.Context, string, string) (net.Conn, error)
	OpenWorkspaceTCP(context.Context, string, uint32) (net.Conn, error)
}
