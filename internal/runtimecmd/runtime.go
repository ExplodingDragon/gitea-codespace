// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"

	"github.com/pkg/sftp"
)

// Connect bridges a stream to an allowed loopback address.
func Connect(ctx context.Context, host string, port uint16, stdin io.Reader, stdout io.Writer) error {
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("runtime TCP host must be localhost")
	}
	if port == 0 {
		return fmt.Errorf("runtime TCP port is invalid")
	}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return fmt.Errorf("connect Dev Container localhost port: %w", err)
	}
	defer func() { _ = connection.Close() }()
	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(connection, stdin)
		if stream, ok := connection.(*net.TCPConn); ok {
			_ = stream.CloseWrite()
		}
		copyDone <- err
	}()
	_, outputErr := io.Copy(stdout, connection)
	inputErr := <-copyDone
	return errors.Join(inputErr, outputErr)
}

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
