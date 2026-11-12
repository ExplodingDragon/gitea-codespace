// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/containernetworking/plugins/pkg/ns"
	"golang.org/x/sys/unix"
)

var usbIPSourcePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

// reverseForwards owns listeners and connections across all SSH transports in
// this runtime. A stopped ordinary listener can retain established channels.
type reverseForwards struct {
	mu          sync.Mutex
	listeners   map[string]*reverseListener
	connections int
}

type reverseListener struct {
	ctx        context.Context
	cancel     context.CancelFunc
	id, source string
	port       uint32
	listeners  []net.Listener
	pending    map[string]*pendingReverseConn
	active     map[string]net.Conn
	stopped    bool
}

type pendingReverseConn struct {
	connection net.Conn
	timer      *time.Timer
}

func (r *Runtime) listenReverse(ctx context.Context, target *agentv1.AccessTarget, request *agentv1.ReverseTCPListen, stream tcpAccessStream) error {
	if request.ListenerId == "" {
		return connect.NewError(connect.CodeInvalidArgument, "reverse listener ID is required")
	}
	var listeners []net.Listener
	var err error
	source := request.GetUsbipSource()
	switch requested := request.Target.(type) {
	case *agentv1.ReverseTCPListen_UsbipSource:
		if !r.USBIP.Enabled() || !usbIPSourcePattern.MatchString(source) {
			return connect.NewError(connect.CodeFailedPrecondition, "USB/IP source is unavailable or invalid")
		}
		listeners, err = listenTCPBind(ctx, "127.0.0.1", 0)
	case *agentv1.ReverseTCPListen_Container:
		if requested.Container == nil || requested.Container.Port > 65535 {
			return connect.NewError(connect.CodeInvalidArgument, "reverse TCP bind is invalid")
		}
		namespace, openErr := containerNetworkNamespace(ctx, target.PrimaryContainerId)
		if openErr != nil {
			return fmt.Errorf("open target container network namespace: %w", openErr)
		}
		err = namespace.Do(func(ns.NetNS) error {
			listeners, err = listenTCPBind(ctx, requested.Container.Address, requested.Container.Port)
			return err
		})
		_ = namespace.Close()
	default:
		return connect.NewError(connect.CodeInvalidArgument, "reverse listener target is required")
	}
	if err != nil {
		for _, listener := range listeners {
			_ = listener.Close()
		}
		return fmt.Errorf("bind reverse listener: %w", err)
	}
	state := &reverseListener{id: request.ListenerId, source: source, port: uint32(listeners[0].Addr().(*net.TCPAddr).Port), listeners: listeners, pending: map[string]*pendingReverseConn{}, active: map[string]net.Conn{}}
	// Serialize registration with target replacement so an in-flight request
	// cannot publish a listener in a container that has just been retired.
	r.mu.RLock()
	if r.target == nil || r.target.PrimaryContainerId != target.PrimaryContainerId || r.target.Version != target.Version {
		err = connect.NewError(connect.CodeFailedPrecondition, "runtime access target changed")
	} else {
		err = r.forwards.register(state)
	}
	r.mu.RUnlock()
	if err != nil {
		for _, listener := range listeners {
			_ = listener.Close()
		}
		return err
	}
	return r.forwards.serve(ctx, state, stream)
}

// Literal addresses avoid DNS and make the network namespace of the socket
// explicit. Default loopback and wildcard bindings use both available families.
func listenTCPBind(ctx context.Context, address string, port uint32) (listeners []net.Listener, resultErr error) {
	addresses := []string{address}
	switch address {
	case "localhost":
		addresses = []string{"127.0.0.1", "::1"}
	case "", "*":
		addresses = []string{"0.0.0.0", "::"}
	default:
		if net.ParseIP(address) == nil {
			return nil, fmt.Errorf("bind address must be localhost, a wildcard or an IP address")
		}
	}
	defer func() {
		if resultErr != nil {
			for _, listener := range listeners {
				_ = listener.Close()
			}
		}
	}()
	for _, host := range addresses {
		network := "tcp6"
		if net.ParseIP(host).To4() != nil {
			network = "tcp4"
		}
		listener, err := (&net.ListenConfig{}).Listen(ctx, network, net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10)))
		if err != nil {
			if len(addresses) > 1 && (errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EADDRNOTAVAIL)) {
				continue
			}
			return listeners, err
		}
		listeners = append(listeners, listener)
		port = uint32(listener.Addr().(*net.TCPAddr).Port)
	}
	if len(listeners) == 0 {
		return nil, fmt.Errorf("no address family is available for %q", address)
	}
	return listeners, nil
}

func (r *reverseForwards) register(state *reverseListener) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listeners == nil {
		r.listeners = map[string]*reverseListener{}
	}
	for id, current := range r.listeners {
		if id == state.id || (state.source != "" && !current.stopped && current.source == state.source) {
			return connect.NewError(connect.CodeAlreadyExists, "reverse listener or USB/IP source is already registered")
		}
	}
	r.listeners[state.id] = state
	state.ctx, state.cancel = context.WithCancel(context.Background())
	return nil
}

func (r *reverseForwards) serve(ctx context.Context, state *reverseListener, stream tcpAccessStream) error {
	graceful := false
	defer func() { r.stop(state, !graceful || state.source != "") }()
	if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ReverseTcpReady{ReverseTcpReady: &agentv1.ReverseTCPReady{Port: state.port}}}); err != nil {
		return err
	}
	type accepted struct {
		connection net.Conn
		err        error
	}
	connections := make(chan accepted)
	for _, listener := range state.listeners {
		go func() {
			for {
				connection, err := listener.Accept()
				select {
				case connections <- accepted{connection, err}:
				case <-state.ctx.Done():
					if connection != nil {
						_ = connection.Close()
					}
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	control := make(chan error, 1)
	go func() {
		request, err := stream.Receive()
		if err == nil && !request.GetStopListening() {
			err = fmt.Errorf("expected stop-listening control frame")
		}
		control <- err
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-state.ctx.Done():
			return connect.NewError(connect.CodeUnavailable, "runtime reverse listener closed")
		case err := <-control:
			if err != nil {
				return err
			}
			graceful = true
			r.stop(state, state.source != "")
			return stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ExitStatus{ExitStatus: 0}})
		case event := <-connections:
			if event.err != nil {
				return event.err
			}
			id := rand.Text()
			r.mu.Lock()
			if state.stopped || r.connections >= 64 {
				r.mu.Unlock()
				_ = event.connection.Close()
				continue
			}
			pending := &pendingReverseConn{connection: event.connection}
			state.pending[id] = pending
			r.connections++
			// A failed ticket or channel setup must not occupy a pending slot for
			// the lifetime of the listener. Established streams have no such timer.
			pending.timer = time.AfterFunc(30*time.Second, func() {
				r.mu.Lock()
				defer r.mu.Unlock()
				if pending := state.pending[id]; pending != nil {
					_ = pending.connection.Close()
					delete(state.pending, id)
					r.connections--
				}
			})
			r.mu.Unlock()
			address := event.connection.RemoteAddr().(*net.TCPAddr)
			if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ReverseTcpConnection{ReverseTcpConnection: &agentv1.ReverseTCPConnection{ConnectionId: id, OriginHost: address.IP.String(), OriginPort: uint32(address.Port)}}}); err != nil {
				return err
			}
		}
	}
}

func (r *reverseForwards) accept(listenerID, connectionID string) (net.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.listeners[listenerID]
	if state == nil || state.stopped || state.pending[connectionID] == nil {
		return nil, connect.NewError(connect.CodeNotFound, "reverse connection is no longer available")
	}
	pending := state.pending[connectionID]
	pending.timer.Stop()
	connection := pending.connection
	delete(state.pending, connectionID)
	state.active[connectionID] = connection
	return &reverseConn{Conn: connection, registry: r, listener: state, id: connectionID}, nil
}

func (r *reverseForwards) stop(state *reverseListener, closeActive bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked(state, closeActive)
}

func (r *reverseForwards) stopLocked(state *reverseListener, closeActive bool) {
	if !state.stopped {
		state.stopped = true
		state.cancel()
		for _, listener := range state.listeners {
			_ = listener.Close()
		}
	}
	for id, pending := range state.pending {
		pending.timer.Stop()
		_ = pending.connection.Close()
		delete(state.pending, id)
		r.connections--
	}
	if closeActive {
		for id, connection := range state.active {
			_ = connection.Close()
			delete(state.active, id)
			r.connections--
		}
	}
	if len(state.active) == 0 && r.listeners[state.id] == state {
		delete(r.listeners, state.id)
	}
}

func (r *reverseForwards) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, state := range r.listeners {
		r.stopLocked(state, true)
	}
}

func (r *reverseForwards) sources() []*agentv1.USBIPSource {
	r.mu.Lock()
	defer r.mu.Unlock()
	sources := make([]*agentv1.USBIPSource, 0)
	for _, state := range r.listeners {
		if state.source != "" && !state.stopped {
			sources = append(sources, &agentv1.USBIPSource{Name: state.source, Port: state.port, ListenerId: state.id})
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	return sources
}

func (r *reverseForwards) sourceContext(ctx context.Context, source *agentv1.USBIPSource) (context.Context, context.CancelFunc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, state := range r.listeners {
		if !state.stopped && state.id == source.ListenerId && state.source == source.Name && state.port == source.Port {
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(state.ctx, cancel)
			return ctx, func() { stop(); cancel() }, nil
		}
	}
	return nil, nil, connect.NewError(connect.CodeNotFound, "USB/IP source disconnected")
}

type reverseConn struct {
	net.Conn
	registry *reverseForwards
	listener *reverseListener
	id       string
}

func (c *reverseConn) Close() error {
	c.registry.mu.Lock()
	if c.listener.active[c.id] != nil {
		delete(c.listener.active, c.id)
		c.registry.connections--
		if c.listener.stopped && len(c.listener.active) == 0 && c.registry.listeners[c.listener.id] == c.listener {
			delete(c.registry.listeners, c.listener.id)
		}
	}
	c.registry.mu.Unlock()
	return c.Conn.Close()
}

func (c *reverseConn) CloseWrite() error { return c.Conn.(*net.TCPConn).CloseWrite() }
