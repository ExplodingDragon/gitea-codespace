// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	RuntimeTarget(codespaceUUID string) (runtimeSnapshot, bool)
}

type sshServer struct {
	config             *ssh.ServerConfig
	state              gatewayWorkspaceTargetStore
	backend            workspaceBackend
	controlPlane       gatewayControlPlaneClient
	sessions           *sessionRegistry
	access             *accessController
	authLimiter        *gatewaySSHAuthLimiter
	handshakeTimeout   time.Duration
	sessionIdleTimeout time.Duration
	revalidateInterval time.Duration
	maxChannels        int
}

type gatewaySSHAuthContext struct {
	codespaceUUID string
	siteUID       string
	userID        int64
	clientIP      string
}

func newSSHServer(
	hostKey ssh.Signer,
	state gatewayWorkspaceTargetStore,
	backend workspaceBackend,
	controlPlane gatewayControlPlaneClient,
	sessions *sessionRegistry,
	access *accessController,
	gatewayConfig configpkg.GatewayConfig,
) (*sshServer, error) {
	if hostKey == nil {
		return nil, fmt.Errorf("gateway ssh host key is required")
	}
	idleTimeout := gatewayConfig.Sessions.IdleTimeout.ToStdlib()
	revalidateInterval := gatewayConfig.Sessions.RevalidateInterval.ToStdlib()
	maxChannels := gatewayConfig.SSH.MaxChannelsPerConnection
	handshakeTimeout := gatewayConfig.SSH.HandshakeTimeout.ToStdlib()
	if idleTimeout <= 0 || revalidateInterval <= 0 || maxChannels <= 0 || handshakeTimeout <= 0 {
		return nil, fmt.Errorf("gateway SSH configuration is incomplete")
	}
	server := &sshServer{
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

func (s *sshServer) authenticatePublicKey(ctx context.Context, conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	sourceIP := gatewaySSHSourceIP(conn.RemoteAddr())
	publicKeyHash := gatewaySSHPublicKeyHash(key)
	codespaceUUID, ok := codespaceUUIDFromGatewaySSHUser(conn.User())
	if !ok {
		s.authLimiter.RecordFailure(sourceIP, "", publicKeyHash, "invalid_credentials", time.Now())
		gatewayLog(ctx, slog.LevelDebug, "gateway_ssh_authentication_failed",
			slog.String("client_ip", sourceIP), slog.String("protocol", "ssh"),
			slog.String("public_key_fingerprint", publicKeyHash), slog.String("reason", "invalid_credentials"))
		return nil, fmt.Errorf("invalid codespace ssh user")
	}
	if !s.authLimiter.Allow(sourceIP, codespaceUUID, publicKeyHash, time.Now()) {
		gatewayLog(ctx, slog.LevelDebug, "gateway_ssh_authentication_failed",
			slog.String("client_ip", sourceIP), slog.String("protocol", "ssh"),
			slog.String("codespace_uuid", codespaceUUID), slog.String("public_key_fingerprint", publicKeyHash),
			slog.String("reason", "rate_limit_reached"))
		return nil, gatewaySSHAuthLimitError()
	}
	if s.controlPlane == nil {
		gatewayLog(ctx, slog.LevelWarn, "gateway_authorization_failed",
			slog.String("client_ip", sourceIP), slog.String("protocol", "ssh"),
			slog.String("codespace_uuid", codespaceUUID), slog.String("stage", "authorization"),
			slog.String("reason", "control_plane_not_ready"))
		return nil, fmt.Errorf("gateway control plane is not ready")
	}
	decision, err := s.controlPlane.VerifySSHPublicKey(ctx, codespaceUUID, key.Marshal())
	if err != nil {
		gatewayLog(ctx, slog.LevelWarn, "gateway_authorization_failed",
			slog.String("client_ip", sourceIP), slog.String("protocol", "ssh"),
			slog.String("codespace_uuid", codespaceUUID), slog.String("stage", "authorization"),
			slog.Any("error", err))
		return nil, err
	}
	if !decision.Allowed {
		s.authLimiter.RecordFailure(sourceIP, codespaceUUID, publicKeyHash, decision.deniedCategory, time.Now())
		gatewayLog(ctx, slog.LevelDebug, "gateway_ssh_authentication_failed",
			slog.String("client_ip", sourceIP), slog.String("protocol", "ssh"),
			slog.String("codespace_uuid", codespaceUUID), slog.String("public_key_fingerprint", publicKeyHash),
			slog.String("reason", decision.deniedCategory))
		return nil, fmt.Errorf("ssh public key denied: %s", decision.deniedCategory)
	}
	if !s.runtimeAvailable(codespaceUUID, decision.SiteUID) {
		return nil, fmt.Errorf("codespace workspace is unavailable")
	}
	return &ssh.Permissions{
		Extensions: map[string]string{
			"codespace_uuid": codespaceUUID,
			"site_uid":       decision.SiteUID,
			"user_id":        formatInt64(decision.UserID),
		},
	}, nil
}

func (s *sshServer) serveConn(ctx context.Context, conn net.Conn) {
	connectionStarted := time.Now()
	clientIP := gatewaySSHSourceIP(conn.RemoteAddr())
	defer func() { _ = conn.Close() }()
	releaseTransport, ok := s.reserveTransport()
	if !ok {
		gatewayLog(ctx, slog.LevelWarn, "gateway_ssh_connection_rejected",
			slog.String("client_ip", clientIP), slog.String("protocol", "ssh"),
			slog.String("reason", "gateway_capacity_unavailable"))
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
		gatewayLog(ctx, slog.LevelDebug, "gateway_ssh_handshake_failed",
			slog.String("client_ip", clientIP), slog.String("protocol", "ssh"),
			slog.Int64("duration_ms", time.Since(connectionStarted).Milliseconds()), slog.Any("error", err))
		return
	}
	_ = conn.SetDeadline(time.Time{})
	defer func() { _ = sshConn.Close() }()
	go ssh.DiscardRequests(requests)

	auth, ok := gatewaySSHAuthFromPermissions(sshConn.Permissions)
	if !ok {
		return
	}
	auth.clientIP = clientIP
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go closeGatewaySSHConnOnDone(sessionCtx, sshConn)
	release := func() {}
	if s.sessions != nil {
		var ok bool
		release, ok = s.sessions.BeginSSHSession(auth.siteUID, auth.codespaceUUID, auth.userID, cancel, time.Now())
		if !ok {
			gatewayLog(ctx, slog.LevelWarn, "gateway_ssh_session_rejected",
				slog.String("client_ip", auth.clientIP), slog.String("protocol", "ssh"),
				slog.String("site_uid", auth.siteUID), slog.String("codespace_uuid", auth.codespaceUUID),
				slog.Int64("user_id", auth.userID), slog.String("reason", "session_limit_reached"))
			return
		}
	}
	defer release()
	runtime, available := s.runtimeTarget(auth.codespaceUUID, auth.siteUID)
	if !available {
		return
	}
	sessionStarted := time.Now()
	sshLogAttrs := gatewaySSHLogAttrs(auth, runtime)
	gatewayLog(sessionCtx, slog.LevelInfo, "gateway_ssh_session_started", sshLogAttrs...)
	defer func() {
		attrs := append([]slog.Attr{}, sshLogAttrs...)
		attrs = append(attrs, slog.Int64("duration_ms", time.Since(sessionStarted).Milliseconds()))
		gatewayLog(context.Background(), slog.LevelInfo, "gateway_ssh_session_ended", attrs...)
	}()
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

func (s *sshServer) reserveTransport() (func(), bool) {
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

func (s *sshServer) cancelIdleSession(ctx context.Context, cancel context.CancelFunc, activity <-chan struct{}) {
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

func (s *sshServer) handleChannel(ctx context.Context, auth gatewaySSHAuthContext, channel ssh.NewChannel, activity chan<- struct{}) {
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

	if !s.runtimeAvailable(auth.codespaceUUID, auth.siteUID) {
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
		case "shell":
			session, err := s.backend.OpenWorkspaceShell(ctx, workspaceShellRequest{
				RuntimeUUID: auth.codespaceUUID,
				Interactive: pty.enabled,
				Cols:        pty.cols,
				Rows:        pty.rows,
			})
			if err != nil {
				s.logSSHBackendFailure(ctx, auth, "shell", 0, err)
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
		case "exec":
			command := parseGatewaySSHExecCommand(request.Payload)
			if command == "" {
				if request.WantReply {
					_ = request.Reply(false, nil)
				}
				continue
			}
			session, err := s.backend.OpenWorkspaceCommand(ctx, workspaceCommandRequest{
				RuntimeUUID: auth.codespaceUUID,
				Command:     command,
				Interactive: pty.enabled,
				Cols:        pty.cols,
				Rows:        pty.rows,
			})
			if err != nil {
				s.logSSHBackendFailure(ctx, auth, "exec", 0, err)
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
			conn, err := s.backend.OpenWorkspaceSFTP(ctx, workspaceSFTPRequest{
				RuntimeUUID: auth.codespaceUUID,
			})
			if err != nil {
				s.logSSHBackendFailure(ctx, auth, "sftp", 0, err)
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

func (s *sshServer) runtimeAvailable(codespaceUUID, siteUID string) bool {
	_, available := s.runtimeTarget(codespaceUUID, siteUID)
	return available
}

func (s *sshServer) runtimeTarget(codespaceUUID, siteUID string) (runtimeSnapshot, bool) {
	if s.state == nil || s.backend == nil {
		return runtimeSnapshot{}, false
	}
	runtime, found := s.state.RuntimeTarget(codespaceUUID)
	return runtime, found && runtime.SiteUID == siteUID
}

func (s *sshServer) handleDirectTCPIP(ctx context.Context, auth gatewaySSHAuthContext, channel ssh.NewChannel, activity chan<- struct{}) {
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
	if !s.runtimeAvailable(auth.codespaceUUID, auth.siteUID) {
		_ = channel.Reject(ssh.ConnectionFailed, "workspace tcp target unavailable")
		return
	}
	backendConn, err := s.backend.OpenWorkspaceTCP(ctx, auth.codespaceUUID, payload.Port)
	if err != nil {
		s.logSSHBackendFailure(ctx, auth, "direct_tcpip", payload.Port, err)
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

func (s *sshServer) logSSHBackendFailure(ctx context.Context, auth gatewaySSHAuthContext, operation string, port uint32, err error) {
	runtime, _ := s.runtimeTarget(auth.codespaceUUID, auth.siteUID)
	attrs := gatewaySSHLogAttrs(auth, runtime)
	attrs = append(attrs,
		slog.String("operation", operation),
		slog.String("stage", gatewayUpstreamStage(err, "agent_open")),
		slog.Any("error", err),
	)
	if port > 0 {
		attrs = append(attrs, slog.Uint64("target_port", uint64(port)))
	}
	if target, ok := gatewayUpstreamTarget(err); ok {
		attrs = append(attrs, slog.GroupAttrs("issued_target", gatewayRuntimeLogAttrs(target)...))
	}
	gatewayLog(ctx, slog.LevelWarn, "gateway_ssh_backend_failed", attrs...)
}

func (s *sshServer) revalidateSession(ctx context.Context, auth gatewaySSHAuthContext, cancel context.CancelFunc) {
	ticker := time.NewTicker(s.revalidateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.controlPlane == nil {
				runtime, _ := s.runtimeTarget(auth.codespaceUUID, auth.siteUID)
				attrs := gatewaySSHLogAttrs(auth, runtime)
				attrs = append(attrs, slog.String("stage", "authorization"), slog.String("reason", "control_plane_not_ready"))
				gatewayLog(ctx, slog.LevelWarn, "gateway_authorization_failed", attrs...)
				cancel()
				return
			}
			decision, err := s.controlPlane.RevalidateSSHSession(ctx, auth.siteUID, auth.userID, auth.codespaceUUID)
			if err != nil {
				runtime, _ := s.runtimeTarget(auth.codespaceUUID, auth.siteUID)
				attrs := gatewaySSHLogAttrs(auth, runtime)
				attrs = append(attrs, slog.String("stage", "authorization"), slog.Any("error", err))
				gatewayLog(ctx, slog.LevelWarn, "gateway_authorization_failed", attrs...)
				cancel()
				return
			}
			if !decision.Allowed {
				runtime, _ := s.runtimeTarget(auth.codespaceUUID, auth.siteUID)
				attrs := gatewaySSHLogAttrs(auth, runtime)
				attrs = append(attrs, slog.String("stage", "authorization"), slog.String("reason", decision.DeniedCategory))
				gatewayLog(ctx, slog.LevelInfo, "gateway_authorization_revoked", attrs...)
				cancel()
				return
			}
		}
	}
}

func serveSSH(ctx context.Context, errorChannel chan<- error, listener net.Listener, server *sshServer) {
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
