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
	"time"

	"github.com/pkg/sftp"
)

// Connect bridges a stream to an allowed loopback address.
func Connect(ctx context.Context, host string, port uint16, wait time.Duration, stdin io.Reader, stdout io.Writer) error {
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("runtime TCP host must be localhost")
	}
	if port == 0 {
		return fmt.Errorf("runtime TCP port is invalid")
	}
	if wait < 0 || wait > 30*time.Second {
		return fmt.Errorf("runtime TCP wait must be between zero and 30 seconds")
	}
	dialContext := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		dialContext, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	var dialer net.Dialer
	address := net.JoinHostPort(host, strconv.Itoa(int(port)))
	var connection net.Conn
	for {
		var err error
		connection, err = dialer.DialContext(dialContext, "tcp", address)
		if err == nil {
			break
		}
		if wait == 0 {
			return fmt.Errorf("connect Dev Container localhost port: %w", err)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-timer.C:
		case <-dialContext.Done():
			timer.Stop()
			return fmt.Errorf("connect Dev Container localhost port: %w", errors.Join(err, dialContext.Err()))
		}
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
