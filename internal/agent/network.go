// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"connectrpc.com/connect/v2"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/docker/docker/client"
)

func containerNetworkNamespace(ctx context.Context, containerID string) (ns.NetNS, error) {
	api, err := client.NewClientWithOpts(client.WithHost(DockerHost), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	defer api.Close()
	info, err := api.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, err
	}
	if info.State == nil || !info.State.Running || info.State.Pid <= 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "target container is not running")
	}
	return ns.GetNS(fmt.Sprintf("/proc/%d/ns/net", info.State.Pid))
}

// networkAccessContext binds pending dials and established streams to the
// Runtime lifecycle, independently of an individual SSH reverse listener.
func (r *Runtime) networkAccessContext(ctx context.Context, target *agentv1.AccessTarget) (context.Context, context.CancelFunc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.target == nil || r.target.PrimaryContainerId != target.PrimaryContainerId || r.target.Version != target.Version {
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, "runtime access target changed")
	}
	if r.networkCtx == nil {
		r.networkCtx, r.cancelNetwork = context.WithCancel(context.Background())
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.networkCtx, cancel)
	return ctx, func() { stop(); cancel() }, nil
}

func (r *Runtime) connectTCP(ctx context.Context, target *agentv1.AccessTarget, containerID string, port uint32, stream tcpAccessStream) error {
	if containerID == "" || port == 0 || port > 65535 {
		return connect.NewError(connect.CodeInvalidArgument, "TCP target is invalid")
	}
	namespace, err := containerNetworkNamespace(ctx, containerID)
	if err != nil {
		return fmt.Errorf("open target container network namespace: %w", err)
	}
	var connection net.Conn
	err = namespace.Do(func(ns.NetNS) error {
		connection, err = dialLoopback(ctx, port)
		return err
	})
	_ = namespace.Close()
	if err != nil {
		if connection != nil {
			_ = connection.Close()
		}
		return fmt.Errorf("connect target container port %d: %w", port, err)
	}
	defer connection.Close()
	r.mu.RLock()
	current := r.target != nil && r.target.PrimaryContainerId == target.PrimaryContainerId && r.target.Version == target.Version
	r.mu.RUnlock()
	if !current {
		return connect.NewError(connect.CodeFailedPrecondition, "runtime access target changed")
	}
	return bridgeTCPConnection(ctx, stream, connection)
}

// Call on the namespace-locked thread. Literal, single-family addresses avoid
// DNS and Happy Eyeballs goroutines, which do not inherit that namespace.
func dialLoopback(ctx context.Context, port uint32) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var failures []error
	for _, address := range []struct{ network, host string }{{"tcp4", "127.0.0.1"}, {"tcp6", "::1"}} {
		connection, err := (&net.Dialer{}).DialContext(ctx, address.network, net.JoinHostPort(address.host, strconv.FormatUint(uint64(port), 10)))
		if err == nil {
			return connection, nil
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(failures...)
}
