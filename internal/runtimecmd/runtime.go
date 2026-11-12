// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/pkg/sftp"
)

// SFTP serves the container filesystem as the Docker exec user. The initial
// directory is a convenience; normal Unix permissions remain the boundary.
func SFTP(workdir string, stdin io.Reader, stdout io.Writer) error {
	if !filepath.IsAbs(workdir) {
		return fmt.Errorf("SFTP working directory must be absolute")
	}
	stream := &readWriteCloser{Reader: stdin, Writer: stdout}
	server, err := sftp.NewServer(stream, sftp.WithServerWorkingDirectory(workdir))
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()
	if err := server.Serve(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

type readWriteCloser struct {
	io.Reader
	io.Writer
}

func (*readWriteCloser) Close() error { return nil }
