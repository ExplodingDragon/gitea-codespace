// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"io"
	"net"
	"strconv"
)

// WorkspaceCommandRequest describes one command stream opened by SSH.
type WorkspaceCommandRequest struct {
	RuntimeUUID string
	Command     string
	Interactive bool
	Cols        int
	Rows        int
}

// WorkspaceSFTPRequest describes one SFTP stream opened by SSH.
type WorkspaceSFTPRequest struct {
	RuntimeUUID string
}

// WorkspaceCommandSession is the bidirectional command stream presented to SSH.
type WorkspaceCommandSession interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Resize(cols, rows int) error
	Signal(signal int) error
	Wait() error
	Close() error
}

// WorkspaceCommandExitError carries a process exit status through the SSH adapter.
type WorkspaceCommandExitError struct {
	Status int
}

func (e *WorkspaceCommandExitError) Error() string {
	return "workspace command exited with status " + strconv.Itoa(e.Status)
}

// WorkspaceBackend opens all supported streams to one Runtime Agent.
type WorkspaceBackend interface {
	OpenWorkspaceCommand(context.Context, WorkspaceCommandRequest) (WorkspaceCommandSession, error)
	OpenWorkspaceSFTP(context.Context, WorkspaceSFTPRequest) (io.ReadWriteCloser, error)
	OpenWorkspaceEndpoint(context.Context, string, string) (net.Conn, error)
	OpenWorkspaceTCP(context.Context, string, uint32) (net.Conn, error)
}
