// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"time"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/pkg/sftp"
	"google.golang.org/protobuf/encoding/protodelim"
)

// Connect bridges framed Agent I/O to a loopback TCP connection. Control input
// stays open after logical stdin EOF so detaching the Agent always ends the helper.
func Connect(ctx context.Context, host string, port uint16, stdin io.ReadCloser, stdout io.Writer) error {
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("runtime TCP host must be localhost")
	}
	if port == 0 {
		return fmt.Errorf("runtime TCP port is invalid")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	stopInput := context.AfterFunc(ctx, func() { _ = stdin.Close() })
	defer stopInput()
	connected := make(chan *net.TCPConn, 1)
	inputEOF := make(chan struct{})
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		reader := bufio.NewReader(stdin)
		var connection *net.TCPConn
		closed := false
		for {
			frame := &agentv1.AccessRequest{}
			err := (protodelim.UnmarshalOptions{MaxSize: 256 * 1024}).UnmarshalFrom(reader, frame)
			if err == nil && connection == nil {
				select {
				case connection = <-connected:
				case <-ctx.Done():
					return
				}
			}
			if err == nil {
				switch value := frame.Frame.(type) {
				case *agentv1.AccessRequest_Stdin:
					if closed {
						err = fmt.Errorf("TCP input is already closed")
					} else {
						_, err = connection.Write(value.Stdin)
					}
				case *agentv1.AccessRequest_StdinEof:
					if closed || !value.StdinEof {
						err = fmt.Errorf("invalid TCP input EOF")
					} else if err = connection.CloseWrite(); err == nil {
						closed = true
						close(inputEOF)
					}
				default:
					err = fmt.Errorf("invalid TCP input frame")
				}
			}
			if err != nil {
				cancel(fmt.Errorf("TCP control input: %w", err))
				return
			}
		}
	}()
	defer func() {
		cancel(nil)
		_ = stdin.Close()
		<-inputDone
	}()
	dialer := net.Dialer{Timeout: 15 * time.Second}
	address := net.JoinHostPort(host, strconv.Itoa(int(port)))
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect Dev Container localhost port: %w", errors.Join(err, context.Cause(ctx)))
	}
	defer func() { _ = connection.Close() }()
	stopConnection := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopConnection()
	connected <- connection.(*net.TCPConn)
	if _, err := protodelim.MarshalTo(stdout, &agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Accepted{Accepted: true}}); err != nil {
		return err
	}
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := connection.Read(buffer)
		if n > 0 {
			if _, err := protodelim.MarshalTo(stdout, &agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Stdout{Stdout: buffer[:n]}}); err != nil {
				return err
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return errors.Join(readErr, context.Cause(ctx))
			}
			if _, err := protodelim.MarshalTo(stdout, &agentv1.AccessResponse{Frame: &agentv1.AccessResponse_StdoutEof{StdoutEof: true}}); err != nil {
				return err
			}
			break
		}
	}
	select {
	case <-inputEOF:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
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
