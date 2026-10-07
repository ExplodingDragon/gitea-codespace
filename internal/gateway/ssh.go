// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"golang.org/x/crypto/ssh"
)

const gatewaySSHUserPrefix = "cs-"
const (
	defaultGatewaySSHCols = 120
	defaultGatewaySSHRows = 40
)

type gatewaySSHPty struct {
	enabled bool
	cols    int
	rows    int
}

type gatewayWorkspaceTargetStore interface {
	RuntimeAvailable(codespaceUUID string) (bool, error)
}

type SSHServer struct {
	config             *ssh.ServerConfig
	state              gatewayWorkspaceTargetStore
	backend            WorkspaceBackend
	controlPlane       gatewayControlPlaneClient
	sessions           *SessionRegistry
	access             *AccessController
	authLimiter        *gatewaySSHAuthLimiter
	handshakeTimeout   time.Duration
	sessionIdleTimeout time.Duration
	revalidateInterval time.Duration
	maxChannels        int
}

type gatewaySSHAuthContext struct {
	codespaceUUID string
	userID        int64
}

func NewSSHServer(
	hostKey ssh.Signer,
	state gatewayWorkspaceTargetStore,
	backend WorkspaceBackend,
	controlPlane gatewayControlPlaneClient,
	sessions *SessionRegistry,
	access *AccessController,
	gatewayConfig configpkg.GatewayConfig,
) (*SSHServer, error) {
	if hostKey == nil {
		return nil, fmt.Errorf("gateway ssh host key is required")
	}
	idleTimeout := gatewayConfig.Sessions.IdleTimeout.ToStdlib()
	if idleTimeout <= 0 {
		idleTimeout = configpkg.DefaultGatewayConfig().Sessions.IdleTimeout.ToStdlib()
	}
	revalidateInterval := gatewayConfig.Sessions.RevalidateInterval.ToStdlib()
	if revalidateInterval <= 0 {
		revalidateInterval = defaultGatewaySessionRevalidateInterval
	}
	maxChannels := gatewayConfig.SSH.MaxChannelsPerConnection
	if maxChannels <= 0 {
		maxChannels = configpkg.DefaultGatewayConfig().SSH.MaxChannelsPerConnection
	}
	handshakeTimeout := gatewayConfig.SSH.HandshakeTimeout.ToStdlib()
	if handshakeTimeout <= 0 {
		handshakeTimeout = configpkg.DefaultGatewayConfig().SSH.HandshakeTimeout.ToStdlib()
	}
	server := &SSHServer{
		state:              state,
		backend:            backend,
		controlPlane:       controlPlane,
		sessions:           sessions,
		access:             access,
		authLimiter:        newGatewaySSHAuthLimiterFromConfig(gatewayConfig),
		handshakeTimeout:   handshakeTimeout,
		sessionIdleTimeout: idleTimeout,
		revalidateInterval: revalidateInterval,
		maxChannels:        maxChannels,
	}
	config := &ssh.ServerConfig{ServerVersion: "SSH-2.0-gitea-codespace"}
	config.AddHostKey(hostKey)
	server.config = config
	return server, nil
}

func (s *SSHServer) authenticatePublicKey(ctx context.Context, conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	sourceIP := gatewaySSHSourceIP(conn.RemoteAddr())
	publicKeyHash := gatewaySSHPublicKeyHash(key)
	codespaceUUID, ok := codespaceUUIDFromGatewaySSHUser(conn.User())
	if !ok {
		s.authLimiter.RecordFailure(sourceIP, "", publicKeyHash, "invalid_credentials", time.Now())
		return nil, fmt.Errorf("invalid codespace ssh user")
	}
	if !s.authLimiter.Allow(sourceIP, codespaceUUID, publicKeyHash, time.Now()) {
		return nil, gatewaySSHAuthLimitError()
	}
	if s.controlPlane == nil {
		return nil, fmt.Errorf("gateway control plane is not ready")
	}
	decision, err := s.controlPlane.VerifySSHPublicKey(ctx, codespaceUUID, key.Marshal())
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		s.authLimiter.RecordFailure(sourceIP, codespaceUUID, publicKeyHash, decision.deniedCategory, time.Now())
		return nil, fmt.Errorf("ssh public key denied: %s", decision.deniedCategory)
	}
	ok, err = s.runtimeAvailable(codespaceUUID)
	if err != nil || !ok {
		return nil, fmt.Errorf("codespace workspace is unavailable")
	}
	return &ssh.Permissions{
		Extensions: map[string]string{
			"codespace_uuid": codespaceUUID,
			"user_id":        formatInt64(decision.UserID),
		},
	}, nil
}

func (s *SSHServer) serveConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	releaseTransport, ok := s.reserveTransport()
	if !ok {
		return
	}
	defer releaseTransport()
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, s.handshakeTimeout)
	handshakeDone := make(chan struct{})
	handshakeWatcherDone := make(chan struct{})
	go func() {
		defer close(handshakeWatcherDone)
		select {
		case <-handshakeCtx.Done():
			_ = conn.Close()
		case <-handshakeDone:
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(s.handshakeTimeout))
	config := *s.config
	config.PublicKeyCallback = func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		return s.authenticatePublicKey(handshakeCtx, metadata, key)
	}
	sshConn, channels, requests, err := ssh.NewServerConn(conn, &config)
	close(handshakeDone)
	<-handshakeWatcherDone
	cancelHandshake()
	if err != nil {
		log.Printf("gateway ssh handshake: %v", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	defer func() { _ = sshConn.Close() }()
	go ssh.DiscardRequests(requests)

	auth, ok := gatewaySSHAuthFromPermissions(sshConn.Permissions)
	if !ok {
		return
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go closeGatewaySSHConnOnDone(sessionCtx, sshConn)
	release := func() {}
	if s.sessions != nil {
		var ok bool
		release, ok = s.sessions.BeginSSHSession(auth.codespaceUUID, auth.userID, cancel, time.Now())
		if !ok {
			return
		}
	}
	defer release()
	if ok, err := s.runtimeAvailable(auth.codespaceUUID); err != nil || !ok {
		return
	}
	activity := make(chan struct{}, 1)
	go s.revalidateSession(sessionCtx, auth, cancel)
	go s.cancelIdleSession(sessionCtx, cancel, activity)
	channelSlots := make(chan struct{}, s.maxChannels)

	for channel := range channels {
		select {
		case channelSlots <- struct{}{}:
		default:
			_ = channel.Reject(ssh.ResourceShortage, "too many ssh channels")
			continue
		}
		notifyGatewaySSHActivity(activity)
		go func() {
			defer func() {
				<-channelSlots
			}()
			s.handleChannel(sessionCtx, auth, channel, activity)
		}()
	}
}

func (s *SSHServer) reserveTransport() (func(), bool) {
	if s.access == nil {
		return func() {}, true
	}
	reservation, status := s.access.ReserveRequest()
	if status != 0 {
		return nil, false
	}
	return reservation.Release, true
}

func closeGatewaySSHConnOnDone(ctx context.Context, conn ssh.Conn) {
	<-ctx.Done()
	_ = conn.Close()
}

func (s *SSHServer) cancelIdleSession(ctx context.Context, cancel context.CancelFunc, activity <-chan struct{}) {
	timer := time.NewTimer(s.sessionIdleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.sessionIdleTimeout)
		case <-timer.C:
			cancel()
			return
		}
	}
}

func (s *SSHServer) handleChannel(ctx context.Context, auth gatewaySSHAuthContext, channel ssh.NewChannel, activity chan<- struct{}) {
	if channel.ChannelType() == "direct-tcpip" {
		s.handleDirectTCPIP(ctx, auth, channel, activity)
		return
	}
	if channel.ChannelType() != "session" {
		_ = channel.Reject(ssh.UnknownChannelType, "unsupported channel type")
		return
	}
	clientChannel, requests, err := channel.Accept()
	if err != nil {
		return
	}
	defer func() { _ = clientChannel.Close() }()

	ok, err := s.runtimeAvailable(auth.codespaceUUID)
	if err != nil || !ok {
		_ = clientChannel.Close()
		return
	}

	pty := gatewaySSHPty{cols: defaultGatewaySSHCols, rows: defaultGatewaySSHRows}
	for request := range requests {
		notifyGatewaySSHActivity(activity)
		switch request.Type {
		case "pty-req":
			pty = parseGatewaySSHPty(request.Payload)
			if request.WantReply {
				_ = request.Reply(true, nil)
			}
		case "window-change":
			pty = parseGatewaySSHWindowChange(request.Payload, pty)
		case "shell", "exec":
			command := ""
			if request.Type == "exec" {
				command = parseGatewaySSHExecCommand(request.Payload)
				if command == "" {
					if request.WantReply {
						_ = request.Reply(false, nil)
					}
					continue
				}
			}
			commandRequest := WorkspaceCommandRequest{RuntimeUUID: auth.codespaceUUID}
			commandRequest.Command = command
			commandRequest.Interactive = pty.enabled
			commandRequest.Cols = pty.cols
			commandRequest.Rows = pty.rows
			session, err := s.backend.OpenWorkspaceCommand(ctx, commandRequest)
			if err != nil {
				if request.WantReply {
					_ = request.Reply(false, nil)
				}
				return
			}
			if request.WantReply {
				_ = request.Reply(true, nil)
			}
			s.serveWorkspaceSession(ctx, clientChannel, requests, session, pty, activity)
			return
		case "subsystem":
			subsystem := parseGatewaySSHSubsystem(request.Payload)
			if subsystem != "sftp" {
				if request.WantReply {
					_ = request.Reply(false, nil)
				}
				continue
			}
			conn, err := s.backend.OpenWorkspaceSFTP(ctx, WorkspaceSFTPRequest{
				RuntimeUUID: auth.codespaceUUID,
			})
			if err != nil {
				if request.WantReply {
					_ = request.Reply(false, nil)
				}
				return
			}
			if request.WantReply {
				_ = request.Reply(true, nil)
			}
			s.proxyWorkspaceStream(ctx, clientChannel, requests, conn, activity)
			return
		default:
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
		}
	}
}

func (s *SSHServer) runtimeAvailable(codespaceUUID string) (bool, error) {
	if s.state == nil || s.backend == nil {
		return false, nil
	}
	return s.state.RuntimeAvailable(codespaceUUID)
}

func (s *SSHServer) handleDirectTCPIP(ctx context.Context, auth gatewaySSHAuthContext, channel ssh.NewChannel, activity chan<- struct{}) {
	payload := struct {
		Host           string
		Port           uint32
		OriginatorHost string
		OriginatorPort uint32
	}{}
	if err := ssh.Unmarshal(channel.ExtraData(), &payload); err != nil ||
		strings.TrimSpace(payload.Host) == "" ||
		payload.Port == 0 ||
		payload.Port > 65535 {
		_ = channel.Reject(ssh.Prohibited, "invalid direct-tcpip target")
		return
	}
	targetHost := strings.TrimSpace(payload.Host)
	switch targetHost {
	case "localhost", "127.0.0.1", "::1":
	default:
		_ = channel.Reject(ssh.Prohibited, "direct-tcpip target must be localhost, 127.0.0.1, or ::1")
		return
	}
	ok, err := s.runtimeAvailable(auth.codespaceUUID)
	if err != nil || !ok {
		_ = channel.Reject(ssh.ConnectionFailed, "workspace tcp target unavailable")
		return
	}
	backendConn, err := s.backend.OpenWorkspaceTCP(ctx, auth.codespaceUUID, payload.Port)
	if err != nil {
		_ = channel.Reject(ssh.ConnectionFailed, "workspace tcp target unavailable")
		return
	}
	clientChannel, requests, err := channel.Accept()
	if err != nil {
		_ = backendConn.Close()
		return
	}
	defer func() { _ = clientChannel.Close() }()
	notifyGatewaySSHActivity(activity)
	s.proxyWorkspaceStream(ctx, clientChannel, requests, backendConn, activity)
}

func (s *SSHServer) revalidateSession(ctx context.Context, auth gatewaySSHAuthContext, cancel context.CancelFunc) {
	ticker := time.NewTicker(s.revalidateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.controlPlane == nil {
				log.Printf("gateway ssh revalidate %s user %d: control plane is not ready", auth.codespaceUUID, auth.userID)
				cancel()
				return
			}
			decision, err := s.controlPlane.RevalidateSSHSession(ctx, auth.userID, auth.codespaceUUID)
			if err != nil {
				log.Printf("gateway ssh revalidate %s user %d: %v", auth.codespaceUUID, auth.userID, err)
				cancel()
				return
			}
			if !decision.Allowed {
				log.Printf("gateway ssh revalidate denied %s user %d: %s", auth.codespaceUUID, auth.userID, decision.DeniedCategory)
				cancel()
				return
			}
		}
	}
}

func ServeSSH(ctx context.Context, errorChannel chan<- error, listener net.Listener, server *SSHServer) {
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) && ctx.Err() != nil {
				return
			}
			errorChannel <- fmt.Errorf("gateway ssh listener: %w", err)
			return
		}
		if server == nil {
			_ = conn.Close()
			continue
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			server.serveConn(ctx, conn)
		}()
	}
}
