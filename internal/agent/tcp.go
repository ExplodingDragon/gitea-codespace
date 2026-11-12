// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
)

type tcpAccessStream interface {
	Receive() (*agentv1.AccessRequest, error)
	Send(*agentv1.AccessResponse) error
}

func bridgeTCPConnection(ctx context.Context, stream tcpAccessStream, connection net.Conn) error {
	defer connection.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Accepted{Accepted: true}}); err != nil {
		return err
	}
	inputDone := make(chan error, 1)
	go func() {
		for {
			request, err := stream.Receive()
			if err != nil {
				_ = connection.Close()
				if errors.Is(err, io.EOF) {
					inputDone <- nil
				} else {
					inputDone <- err
				}
				return
			}
			switch frame := request.Frame.(type) {
			case *agentv1.AccessRequest_Stdin:
				_, err = connection.Write(frame.Stdin)
			case *agentv1.AccessRequest_StdinEof:
				if closer, ok := connection.(interface{ CloseWrite() error }); ok && frame.StdinEof {
					err = closer.CloseWrite()
				} else if !frame.StdinEof {
					err = fmt.Errorf("invalid TCP input EOF")
				}
			default:
				err = fmt.Errorf("invalid TCP input frame")
			}
			if err != nil {
				_ = connection.Close()
				inputDone <- err
				return
			}
			if _, ok := request.Frame.(*agentv1.AccessRequest_StdinEof); ok {
				inputDone <- nil
				return
			}
		}
	}()
	if err := copyFrames(connection, func(value []byte) error {
		return stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Stdout{Stdout: value}})
	}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case inputErr := <-inputDone:
			if inputErr != nil {
				return inputErr
			}
		default:
		}
		return err
	}
	if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_StdoutEof{StdoutEof: true}}); err != nil {
		return err
	}
	// TCP output EOF does not close input: the peer may still be writing.
	select {
	case err := <-inputDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ExitStatus{ExitStatus: 0}})
}
